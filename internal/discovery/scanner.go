package discovery

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/singleflight"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Scanner holds the mutable caches behind the package-level wrappers
// (Scan / LookupSummaries / RefreshDynamic → DefaultScanner()); tests needing
// isolation use NewScanner(). promptCache and summaryCache are hit-result
// caches keyed by path+mtime, aged by Scan's generation counter and bounded
// at 500 entries. pathCache fronts findSessionJSONL's O(projects) fallback
// scan: positive hits re-validate with one os.Stat, negatives expire by TTL.
type Scanner struct {
	promptCache  promptCacheState
	summaryCache summaryCacheState
	pathCache    pathCacheState

	// summaryLoad collapses concurrent loads of the same sessions-index.json
	// into one stat+read+parse, so promptWg goroutines on a cache miss do not
	// serialise behind the slowest parse for the same workspace (#1967).
	summaryLoad singleflight.Group

	// promptSem bounds the prompt-extraction fan-out to promptSemCap. Each
	// worker does a symmetric send-then-receive, so the channel is drained
	// before Wait() returns and can be reused across scan cycles (#2124).
	promptSem chan struct{}
}

// promptSemCap caps concurrent prompt-extraction I/O so a workspace with
// hundreds of sessions does not open hundreds of JSONL files at once.
const promptSemCap = 4

type promptCacheState struct {
	sync.RWMutex
	entries map[string]*promptCacheEntry
	// generation is bumped once per Scan; atomic so getCachedPrompt refreshes a
	// hit entry's gen without a write lock serialising promptWg goroutines (#1966).
	generation atomic.Uint64
}

type promptCacheEntry struct {
	mtime  int64
	prompt string
	// gen is an inline atomic. The map stores *promptCacheEntry so &gen stays
	// stable for the entry's lifetime: getCachedPrompt refreshes it lock-free
	// via Store and evictPromptCache reads it under the write lock (#2322).
	gen atomic.Uint64
}

type summaryCacheState struct {
	sync.RWMutex
	entries map[string]*summaryCacheEntry
	// generation is bumped once per Scan; atomic (like promptCache) so
	// getCachedSummary can refresh a hit entry's gen lock-free (#2330).
	generation atomic.Uint64
}

type summaryCacheEntry struct {
	mtime int64
	index sessionsIndex
	// gen is an inline atomic; the map stores *summaryCacheEntry so &gen stays
	// stable and getCachedSummary can refresh it lock-free on a hit (#2330).
	gen atomic.Uint64
}

// pathCacheState maps (claudeDir, sessionID) to the resolved JSONL path, or to
// a zero-path entry with a negativeUntil deadline when a scan found nothing.
// Hits take RLock; the slow ReadDir+Stat fan-out takes the write lock to commit.
type pathCacheState struct {
	sync.RWMutex
	entries map[pathKey]pathCacheEntry
}

// pathKey is the composite (claudeDir, sessionID) map key; a struct key avoids
// the per-lookup string concatenation a packed "dir\x00id" key costs (#2125).
type pathKey struct {
	dir string
	id  string
}

// pathCacheEntry is a positive result (path != "") or a bounded negative one.
// Positives have no TTL (callers os.Stat-validate and evict on mismatch, so a
// renamed/deleted JSONL self-heals); negatives expire after pathCacheNegativeTTL.
type pathCacheEntry struct {
	path          string
	negativeUntil time.Time
}

// pathCacheNegativeTTL caps how long a "not found" verdict stays cached: long
// enough for a startup burst of resume-chain walks to share one ReadDir pass.
const pathCacheNegativeTTL = 60 * time.Second

// pathCacheMaxEntries bounds the map for long-running processes that see tens
// of thousands of distinct sessionIDs. Expired negatives are dropped first;
// evictPathCacheLocked then falls back to random eviction so the cap always holds.
const pathCacheMaxEntries = 2048

// pathCacheEvictBatch is the headroom evictPathCacheLocked leaves after the
// fallback pass so stores at the cap do not re-evict on every call.
const pathCacheEvictBatch = 16

// maxSessionFileBytes caps Claude session-state files read during Scan; real
// files are a few KB, anything larger is corruption and is skipped.
const maxSessionFileBytes int64 = 1024 * 1024

// readBoundedSessionFile opens path, fstats the descriptor and reads through
// io.LimitReader so an oversized file is rejected without loading it. The
// cap-exceeded case is an error, not a partial read, so callers cannot
// json.Unmarshal a truncated payload. One Open + one fstat per file (#676).
func readBoundedSessionFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSessionFileBytes {
		return nil, fmt.Errorf("session file exceeds %d-byte cap (%d bytes)", maxSessionFileBytes, info.Size())
	}
	// LimitReader is defence-in-depth against a concurrent writer growing the
	// file between Stat and Read.
	data, err := io.ReadAll(io.LimitReader(f, maxSessionFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSessionFileBytes {
		return nil, fmt.Errorf("session file grew past %d-byte cap mid-read", maxSessionFileBytes)
	}
	return data, nil
}

// NewScanner returns a fresh Scanner with empty caches, for tests that need
// isolation; production callers use the wrappers over DefaultScanner.
func NewScanner() *Scanner {
	return &Scanner{
		promptCache:  promptCacheState{entries: make(map[string]*promptCacheEntry)},
		summaryCache: summaryCacheState{entries: make(map[string]*summaryCacheEntry)},
		pathCache:    pathCacheState{entries: make(map[pathKey]pathCacheEntry)},
		promptSem:    make(chan struct{}, promptSemCap),
	}
}

// defaultScannerInst is the process-wide Scanner behind the package-level
// wrappers, lazily initialised so callers that never scan allocate nothing.
var (
	defaultScannerOnce sync.Once
	defaultScannerInst *Scanner
)

// scanUserPromptBufPool recycles the 16 KiB bufio.Scanner buffers used by
// scanUserPrompt, which runs for up to 4 candidates concurrently per Scan.
var scanUserPromptBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 16*1024)
		return &b
	},
}

// userTypeMarker is scanUserPrompt's quick-filter substring, hoisted so the
// []byte literal does not allocate on every line of the hot scan loop.
var userTypeMarker = []byte(`"type":"user"`)

// summaryStatFn is the os.Stat indirection used by loadSummaryIndex; a var so
// tests can count syscalls (#2247).
var summaryStatFn = os.Stat

func DefaultScanner() *Scanner {
	defaultScannerOnce.Do(func() {
		defaultScannerInst = NewScanner()
	})
	return defaultScannerInst
}

// evictPromptCache deletes entries more than one generation old once the cache
// exceeds 500 entries. Caller must hold s.promptCache.Lock().
func (s *Scanner) evictPromptCache() {
	if len(s.promptCache.entries) <= 500 {
		return
	}
	gen := s.promptCache.generation.Load()
	for k, v := range s.promptCache.entries {
		if v.gen.Load()+1 < gen {
			delete(s.promptCache.entries, k)
		}
	}
}

// evictSummaryCache deletes entries more than one generation old once the
// cache exceeds 500 entries. Caller must hold s.summaryCache.Lock().
func (s *Scanner) evictSummaryCache() {
	if len(s.summaryCache.entries) <= 500 {
		return
	}
	gen := s.summaryCache.generation.Load()
	for k, v := range s.summaryCache.entries {
		if v.gen.Load()+1 < gen {
			delete(s.summaryCache.entries, k)
		}
	}
}

// runningThreshold is the JSONL mtime window that classifies a discovered
// process as "running" vs "ready". 30s avoids flapping: the CLI writes JSONL
// during idle housekeeping (compaction, MCP events, index updates).
const runningThreshold = 30 * time.Second

// noJSONLGrace is the window a freshly-started CLI process gets to write its
// first JSONL line before a missing conversation file marks it as an idle
// wrapper: VS Code's Claude extension keeps an extra `claude` child alive for
// the editor's lifetime that never gets a JSONL and would duplicate the sidebar.
const noJSONLGrace = 5 * time.Second

// MaxSafeJSONInt mirrors JavaScript's Number.MAX_SAFE_INTEGER (2^53 - 1). Any
// uint64 that crosses a JSON boundary into dashboard.js or a reverse-RPC peer
// must stay below it or JSON.parse rounds it and PID-identity comparisons
// (ProcStartTime) match wrongly. Producers are far below (Linux jiffies:
// ~2.85M years of uptime; Darwin Unix μs: year ~2255) and proc_*_test.go
// asserts ProcStartTime(os.Getpid()) <= MaxSafeJSONInt to catch encoding changes.
const MaxSafeJSONInt uint64 = (1 << 53) - 1

// DiscoveredSession represents a Claude CLI process found on the system.
type DiscoveredSession struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"session_id"`
	CWD        string `json:"cwd"`
	StartedAt  int64  `json:"started_at"`            // unix ms
	LastActive int64  `json:"last_active"`           // unix ms (from JSONL mtime, fallback to started_at)
	State      string `json:"state"`                 // "running" or "ready"
	Kind       string `json:"kind"`                  // "interactive" etc.
	Entrypoint string `json:"entrypoint"`            // "cli" etc.
	CLIName    string `json:"cli_name,omitempty"`    // "claude-code", "kiro" (detected from process cmdline)
	Summary    string `json:"summary,omitempty"`     // Claude-generated session name from sessions-index
	LastPrompt string `json:"last_prompt,omitempty"` // most recent user message
	// ProcStartTime is a per-PID boot identity that detects PID reuse (Linux:
	// /proc/PID/stat field 22; Darwin: Unix μs from ps lstart). It crosses JSON
	// boundaries and MUST stay below MaxSafeJSONInt or handleTakeover's check fails.
	ProcStartTime uint64 `json:"proc_start_time"`
	Project       string `json:"project,omitempty"` // project name resolved from CWD (filled by server)
	Node          string `json:"node,omitempty"`    // workspace/node ID (filled by server for multi-node)
}

// sessionFile mirrors the JSON schema of ~/.claude/sessions/{PID}.json.
type sessionFile struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	CWD        string `json:"cwd"`
	StartedAt  int64  `json:"startedAt"`
	Kind       string `json:"kind"`
	Entrypoint string `json:"entrypoint"`
}

// scanCandidate holds intermediate state during session scanning.
type scanCandidate struct {
	sf         sessionFile
	lastActive int64
}

// Scan is the package-level wrapper over DefaultScanner. Use (*Scanner).Scan
// directly when you need cache isolation.
func Scan(claudeDir string, excludePIDs map[int]bool, excludeSessionIDs map[string]bool, managedCWDs map[string]bool) ([]DiscoveredSession, error) {
	return DefaultScanner().Scan(claudeDir, excludePIDs, excludeSessionIDs, managedCWDs)
}

// ScanContext is the cancellation-aware variant of Scan: the prompt-extraction
// fan-out does unbounded filesystem IO, so a hung NFS/FUSE mount could stall
// shutdown past systemd TimeoutStopSec without a SIGTERM-derived ctx (#2244).
func ScanContext(ctx context.Context, claudeDir string, excludePIDs map[int]bool, excludeSessionIDs map[string]bool, managedCWDs map[string]bool) ([]DiscoveredSession, error) {
	return DefaultScanner().ScanContext(ctx, claudeDir, excludePIDs, excludeSessionIDs, managedCWDs)
}

// Scan reads ~/.claude/sessions/*.json and returns live Claude CLI processes
// not managed by naozhi (excludePIDs), with SessionID taken verbatim from each
// {pid}.json. excludeSessionIDs and managedCWDs are retained for call-site
// compatibility but not consulted (TestScan_SessionIDNeverUpgradedToOtherSessionJSONL).
func (s *Scanner) Scan(claudeDir string, excludePIDs map[int]bool, excludeSessionIDs map[string]bool, managedCWDs map[string]bool) ([]DiscoveredSession, error) {
	return s.ScanContext(context.Background(), claudeDir, excludePIDs, excludeSessionIDs, managedCWDs)
}

// ScanContext is the cancellation-aware variant of (*Scanner).Scan; see the
// package-level ScanContext.
func (s *Scanner) ScanContext(ctx context.Context, claudeDir string, excludePIDs map[int]bool, excludeSessionIDs map[string]bool, managedCWDs map[string]bool) ([]DiscoveredSession, error) {
	sessDir := filepath.Join(claudeDir, "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	// Advance cache generations once per scan; eviction ages by generation.
	s.promptCache.generation.Add(1)
	s.summaryCache.generation.Add(1)

	var candidates []scanCandidate

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		// Session files are a handful of fields; skip anything pathologically
		// large rather than parse it. One Open + fstat per file (#676).
		data, err := readBoundedSessionFile(filepath.Join(sessDir, entry.Name()))
		if err != nil {
			continue
		}

		var sf sessionFile
		if err := json.Unmarshal(data, &sf); err != nil {
			continue
		}

		if sf.PID <= 0 || sf.SessionID == "" {
			continue
		}

		// Only include CLI/VSCode sessions (skip sdk-ts observers, etc.)
		if sf.Entrypoint != "" && sf.Entrypoint != "cli" && sf.Entrypoint != "claude-vscode" {
			continue
		}

		if excludePIDs[sf.PID] {
			continue
		}

		if !processAlive(sf.PID) {
			continue
		}

		// Gate out idle CLI wrappers that never got a JSONL (VS Code runs one
		// --resume child plus a sessionless wrapper, both with sessions/<pid>.json;
		// the wrapper would duplicate the sidebar card). One Stat serves both the
		// grace gate and lastActive, which falls back to the process start time.
		jsonlPath := filepath.Join(claudeDir, "projects", projDirName(sf.CWD), sf.SessionID+".jsonl")
		la := sf.StartedAt
		if fi, err := os.Stat(jsonlPath); err != nil {
			startedAt := time.UnixMilli(sf.StartedAt)
			if !startedAt.IsZero() && time.Since(startedAt) > noJSONLGrace {
				continue
			}
		} else {
			la = fi.ModTime().UnixMilli()
		}

		candidates = append(candidates, scanCandidate{sf: sf, lastActive: la})
	}

	// No live candidates: skip the map/slice allocations — between active
	// sessions this is the steady-state path on every ~10s scan.
	if len(candidates) == 0 {
		return nil, nil
	}

	candidateWorkspaces := make(map[string]string, len(candidates))
	for i := range candidates {
		candidateWorkspaces[candidates[i].sf.SessionID] = candidates[i].sf.CWD
	}
	summaryMap := s.LookupSummaries(claudeDir, candidateWorkspaces)

	prompts := make([]string, len(candidates))
	var promptWg sync.WaitGroup
	promptSem := s.promptSem
	for i := range candidates {
		promptWg.Add(1)
		go func(idx int) {
			defer promptWg.Done()
			// Abandon not-yet-started extractions on ctx cancellation so a hung
			// FS cannot park this goroutine on promptSem past shutdown; an empty
			// prompts[idx] is the same outcome as a failed extraction (#2244).
			select {
			case promptSem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-promptSem }()
			prompts[idx] = s.extractLastPrompt(claudeDir, candidates[idx].sf.CWD, candidates[idx].sf.SessionID)
		}(i)
	}
	promptWg.Wait()

	nowMs := time.Now().UnixMilli()
	var result []DiscoveredSession
	for i := range candidates {
		c := &candidates[i]
		pst, err := ProcStartTime(c.sf.PID)
		if err != nil {
			slog.Debug("discovery: skip candidate, cannot read proc start time", "pid", c.sf.PID, "err", err)
			continue
		}
		state := "ready"
		if c.lastActive > nowMs-int64(runningThreshold/time.Millisecond) {
			state = "running"
		}
		result = append(result, DiscoveredSession{
			PID:           c.sf.PID,
			SessionID:     c.sf.SessionID,
			CWD:           c.sf.CWD,
			StartedAt:     c.sf.StartedAt,
			LastActive:    c.lastActive,
			State:         state,
			Kind:          c.sf.Kind,
			Entrypoint:    c.sf.Entrypoint,
			CLIName:       detectCLIName(c.sf.PID),
			Summary:       SanitizePromptForTransport(summaryMap[c.sf.SessionID]),
			LastPrompt:    prompts[i],
			ProcStartTime: pst,
		})
	}
	return result, nil
}

// processAlive checks whether a process with the given PID exists. Delegates
// to osutil.PidAlive so the pid<=0 guard (kill(0)/kill(-N) broadcast to groups
// and would misreport phantom processes as alive) is consistent across packages.
func processAlive(pid int) bool {
	return osutil.PidAlive(pid)
}

// claudeSlugMaxLen mirrors the Claude CLI's cap on an encoded project directory
// name: beyond it the CLI truncates and appends "-" + a base36 hash of the
// original path (verified against CLI 2.1.219 with a 40-segment CWD).
const claudeSlugMaxLen = 200

// ClaudeProjectSlug converts a CWD path to the Claude project directory name,
// e.g. "/home/user/workspace/foo" -> "-home-user-workspace-foo"; it is the
// single source of truth for the scheme (internal/session wraps it). Every
// character outside [A-Za-z0-9] becomes '-' per UTF-16 code unit (see
// substituteNonAlnum). Control bytes (< 0x20) are stripped first so hand-edited
// persisted state (cron_jobs.json, sessions-index.json) with embedded \t/\n
// cannot steer the encoded path onto an attacker-prepared directory (#465).
func ClaudeProjectSlug(cwd string) string {
	if hasControlByte(cwd) {
		cwd = stripControlBytes(cwd)
	}
	slug := substituteNonAlnum(cwd)
	if len(slug) <= claudeSlugMaxLen {
		return slug
	}
	return slug[:claudeSlugMaxLen] + "-" + claudeSlugHash(cwd)
}

// substituteNonAlnum replaces everything outside [A-Za-z0-9] with '-',
// mirroring the CLI's `replace(/[^a-zA-Z0-9]/g, "-")` per UTF-16 code unit: a
// BMP rune (CJK ideograph) yields ONE '-', a non-BMP rune (emoji) TWO. Verified
// against CLI 2.1.219 ("/tmp/slugtest2/中文目录" → "-tmp-slugtest2-----").
// Invalid UTF-8 bytes decode as RuneError size 1 and contribute one '-' each.
func substituteNonAlnum(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		if !isASCIIAlnum(s[i]) {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	// strings.Builder hands out its buffer without copying, keeping this at
	// one allocation per call on the sidebar-fetch / cron URL hot paths.
	var b strings.Builder
	// The output is never longer than the input: every ASCII-alnum byte maps
	// to itself, and any multi-byte rune shrinks to at most two dashes.
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if isASCIIAlnum(c) {
				b.WriteByte(c)
			} else {
				b.WriteByte('-')
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r > 0xFFFF {
			b.WriteString("--")
		} else {
			b.WriteByte('-')
		}
		i += size
	}
	return b.String()
}

func isASCIIAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// claudeSlugHash reproduces the CLI's overflow suffix, a Java-style 32-bit
// string hash of the original path rendered base36:
//
//	let t = 0; for (c of s) t = (t << 5) - t + c.charCodeAt(i) | 0
//	Math.abs(t).toString(36)
//
// int32 arithmetic matches JS's `| 0` wraparound; non-BMP runes are folded
// into surrogate pairs; Math.abs(-2^31) is 2^31 in JS, so that input is special-cased.
func claudeSlugHash(s string) string {
	var h int32
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			hi := int32(0xD800 + (r >> 10))
			lo := int32(0xDC00 + (r & 0x3FF))
			h = (h << 5) - h + hi
			h = (h << 5) - h + lo
			continue
		}
		h = (h << 5) - h + int32(r)
	}
	if h == math.MinInt32 {
		// JS: Math.abs(-2147483648) === 2147483648.
		return strconv.FormatUint(1<<31, 36)
	}
	if h < 0 {
		h = -h
	}
	return strconv.FormatInt(int64(h), 36)
}

// hasControlByte reports whether s contains any byte < 0x20 (no allocation).
func hasControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 {
			return true
		}
	}
	return false
}

// stripControlBytes returns s with every byte < 0x20 removed; hasControlByte
// gates it so the typical cwd never pays the copy.
func stripControlBytes(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x20 {
			b = append(b, s[i])
		}
	}
	return string(b)
}

// projDirName is the package-internal alias for ClaudeProjectSlug.
func projDirName(cwd string) string {
	return ClaudeProjectSlug(cwd)
}

// jsonlMtime returns the JSONL conversation file's mtime as unix ms.
// Falls back to startedAt if the file is not found.
func jsonlMtime(claudeDir, cwd, sessionID string, startedAt int64) int64 {
	jsonlPath := filepath.Join(claudeDir, "projects", projDirName(cwd), sessionID+".jsonl")
	info, err := os.Stat(jsonlPath)
	if err != nil {
		return startedAt
	}
	return info.ModTime().UnixMilli()
}

// sessionsIndex mirrors the sessions-index.json schema.
type sessionsIndex struct {
	OriginalPath string               `json:"originalPath"`
	Entries      []sessionsIndexEntry `json:"entries"`
}

type sessionsIndexEntry struct {
	SessionID   string `json:"sessionId"`
	Summary     string `json:"summary"`
	FirstPrompt string `json:"firstPrompt"`
}

// extractLastPrompt reads the JSONL file backwards to find the last user message.
// Results are cached by (path, mtime) to avoid re-reading 512KB every scan cycle.
func (s *Scanner) extractLastPrompt(claudeDir, cwd, sessionID string) string {
	prompt, _, _ := s.extractLastPromptWithMtime(claudeDir, cwd, sessionID)
	return prompt
}

// extractLastPromptWithMtime returns the last prompt plus the JSONL mtime (unix
// ms) and whether the file exists, so RefreshDynamic can reuse this single Stat
// for lastActive instead of a second os.Stat via jsonlMtime.
func (s *Scanner) extractLastPromptWithMtime(claudeDir, cwd, sessionID string) (string, int64, bool) {
	// One Stat resolves both existence and mtime.
	path := filepath.Join(claudeDir, "projects", projDirName(cwd), sessionID+".jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, false
	}
	mtime := fi.ModTime().UnixNano()
	mtimeMs := fi.ModTime().UnixMilli()

	if cached, ok := s.getCachedPrompt(path, mtime); ok {
		return cached, mtimeMs, true
	}

	result := extractLastPromptUncached(path, fi.Size())

	s.setCachedPrompt(path, mtime, result)
	return result, mtimeMs, true
}

// getCachedPrompt checks the prompt cache under RLock; a hit refreshes gen lock-free.
func (s *Scanner) getCachedPrompt(path string, mtime int64) (string, bool) {
	s.promptCache.RLock()
	cached, ok := s.promptCache.entries[path]
	s.promptCache.RUnlock()
	if !ok || cached.mtime != mtime {
		return "", false
	}
	// Refresh gen lock-free so eviction keeps the entry alive; the entry
	// address is stable so no write-lock upgrade is needed (#1966).
	cached.gen.Store(s.promptCache.generation.Load())
	return cached.prompt, true
}

// setCachedPrompt writes a prompt cache entry under a deferred lock.
func (s *Scanner) setCachedPrompt(path string, mtime int64, result string) {
	s.promptCache.Lock()
	defer s.promptCache.Unlock()
	entry := &promptCacheEntry{mtime: mtime, prompt: result}
	entry.gen.Store(s.promptCache.generation.Load())
	s.promptCache.entries[path] = entry
	s.evictPromptCache()
}

// extractLastPromptUncached does the actual 512KB tail read and JSON scanning.
// If the tail window contains only tool_result user messages (no text prompts),
// it falls back to scanning from the beginning of the file.
func extractLastPromptUncached(path string, fileSize int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	const tailSize = 512 * 1024
	offset := fileSize - tailSize
	if offset < 0 {
		offset = 0
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			slog.Warn("seek failed in JSONL preview", "err", err)
		}
	}

	lastPrompt := scanUserPrompt(f)

	// If the tail found no text prompt and earlier content was skipped, re-scan
	// from the start, capped at tailSize too (#2227): the opening prompt lives
	// near the start, and without a cap a 10MB JSONL whose tail is all
	// tool_result lines would be read in full on every preview refresh.
	if lastPrompt == "" && offset > 0 {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			lastPrompt = scanUserPrompt(io.LimitReader(f, tailSize))
		}
	}

	return textutil.TruncateRunes(lastPrompt, 120)
}

// scanUserPrompt scans lines from the current file position and returns
// the last user message that contains actual text (not tool_result, and
// not one of Claude Code's system-injected XML frames).
func scanUserPrompt(r io.Reader) string {
	var lastPrompt string
	scanner := bufio.NewScanner(r)
	bufPtr := scanUserPromptBufPool.Get().(*[]byte)
	// bufio.Scanner may grow the slice; reset length on return (capacity never
	// shrinks below the initial 16 KiB).
	defer func() {
		buf := (*bufPtr)[:0]
		*bufPtr = buf
		scanUserPromptBufPool.Put(bufPtr)
	}()
	scanner.Buffer(*bufPtr, 256*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !bytes.Contains(line, userTypeMarker) {
			continue
		}
		var hl struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &hl) != nil || hl.Type != "user" {
			continue
		}
		text := extractUserText(hl.Message)
		if text == "" || IsClaudeSystemInjectedText(text) {
			continue
		}
		lastPrompt = text
	}
	return SanitizePromptForTransport(lastPrompt)
}

// SanitizePromptForTransport strips bytes that corrupt structured log output,
// terminal rendering, or /api/sessions/resume's charset gate from last_prompt /
// first_prompt strings flowing from a CLI JSONL through the sidebar JSON and
// back to the resume endpoint. Claude CLI emits user messages with control
// bytes (PDF uploads use U+0085 NEL, shell outputs carry C0 noise). Tab is kept:
// tab-delimited snippets are legitimate and slog escapes it. Used by recent.go too.
func SanitizePromptForTransport(s string) string {
	if s == "" {
		return s
	}
	// Rune-level fast path so valid multi-byte UTF-8 (Chinese, emoji) does not
	// fall through to strings.Map; the predicate is identical to the slow path
	// so the two cannot diverge.
	clean := true
	for _, r := range s {
		if r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || osutil.IsLogInjectionRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		if osutil.IsLogInjectionRune(r) {
			return '_'
		}
		return r
	}, s)
}

// claudeSystemInjectedTagNames enumerates the XML-like tags Claude Code and
// its plugins inject as synthetic user messages — operational noise that must
// not become a session title or history entry. Kept in lockstep with the UI
// filter in internal/server/static/dashboard.js (eventHtml + formatSessionMarkdown).
var claudeSystemInjectedTagNames = [...]string{
	"task-notification",
	"system-reminder",
	"local-command",
	"command-name",
	"available-deferred-tools",
}

// IsClaudeSystemInjectedText reports whether text is a Claude-Code-injected
// system XML frame (a leading "<tag>" / "<tag " from claudeSystemInjectedTagNames)
// or the CLI's synthetic "[Request interrupted by user]" marker written when
// SIGINT aborts a turn — neither is user intent for titles, previews or exports.
func IsClaudeSystemInjectedText(text string) bool {
	if isClaudeInterruptMarker(text) {
		return true
	}
	if len(text) < 3 || text[0] != '<' {
		return false
	}
	for _, name := range claudeSystemInjectedTagNames {
		if len(text) < len(name)+2 {
			continue
		}
		if text[1:1+len(name)] != name {
			continue
		}
		next := text[1+len(name)]
		if next == '>' || next == ' ' || next == '\t' || next == '\n' || next == '\r' {
			return true
		}
	}
	return false
}

// isClaudeInterruptMarker matches the two CLI-synthesised interrupt messages
// (SIGINT / stop button) in Claude CLI ≥ 2.1.
func isClaudeInterruptMarker(text string) bool {
	return text == "[Request interrupted by user]" ||
		text == "[Request interrupted by user for tool use]"
}

// extractUserText extracts the text content from a user message.
func extractUserText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil || len(msg.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return ""
}

// findJSONLPath locates the JSONL for a session, trying the CWD-based path first.
func findJSONLPath(claudeDir, cwd, sessionID string) string {
	candidate := filepath.Join(claudeDir, "projects", projDirName(cwd), sessionID+".jsonl")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// LookupSummaries is the package-level wrapper over DefaultScanner.
func LookupSummaries(claudeDir string, sessions map[string]string) map[string]string {
	return DefaultScanner().LookupSummaries(claudeDir, sessions)
}

// LookupSummaries looks up Claude-generated summaries for the given sessions.
// The sessions map is sessionID → workspace (CWD path).
// Returns a map of sessionID → summary.
func (s *Scanner) LookupSummaries(claudeDir string, sessions map[string]string) map[string]string {
	if claudeDir == "" || len(sessions) == 0 {
		return nil
	}

	// Group session IDs by project directory to read each index file once.
	byProjDir := make(map[string][]string, len(sessions)) // indexPath → []sessionID
	for sid, workspace := range sessions {
		if workspace == "" {
			continue
		}
		indexPath := filepath.Join(claudeDir, "projects", projDirName(workspace), "sessions-index.json")
		byProjDir[indexPath] = append(byProjDir[indexPath], sid)
	}

	result := make(map[string]string, len(sessions))
	for indexPath, sids := range byProjDir {
		idx, ok := s.loadSummaryIndex(indexPath)
		if !ok {
			continue
		}

		// Single-session projects (the common case) skip the set allocation;
		// idx.Entries can reach the hundreds, so larger lists use O(1) membership.
		switch len(sids) {
		case 1:
			want := sids[0]
			for _, e := range idx.Entries {
				if e.Summary == "" || e.SessionID != want {
					continue
				}
				result[e.SessionID] = e.Summary
			}
		default:
			sidSet := make(map[string]struct{}, len(sids))
			for _, s := range sids {
				sidSet[s] = struct{}{}
			}
			for _, e := range idx.Entries {
				if e.Summary == "" {
					continue
				}
				if _, ok := sidSet[e.SessionID]; ok {
					result[e.SessionID] = e.Summary
				}
			}
		}
	}
	return result
}

// loadSummaryIndex resolves indexPath to its parsed sessionsIndex, serving the
// summary cache on an mtime hit and otherwise reading + parsing once. Concurrent
// callers for the same indexPath are collapsed via singleflight so they share
// one stat+read+parse (#1967). ok is false when missing or unparseable.
func (s *Scanner) loadSummaryIndex(indexPath string) (sessionsIndex, bool) {
	// Fast path: one stat serves a fresh cache hit without entering
	// singleflight. On a miss the stat's mtime is carried into the flight so
	// the closure does not re-stat the same file (#2247).
	fastMtime, haveFastMtime := int64(0), false
	if fi, err := summaryStatFn(indexPath); err == nil {
		mtime := fi.ModTime().UnixNano()
		fastMtime, haveFastMtime = mtime, true
		if cachedIdx, ok := s.getCachedSummary(indexPath, mtime); ok {
			return cachedIdx, true
		}
	}

	v, err, _ := s.summaryLoad.Do(indexPath, func() (any, error) {
		mtime := fastMtime
		if !haveFastMtime {
			// This caller's fast-path stat failed (transient error); stat inside
			// the flight so a genuine miss still surfaces the file's mtime.
			fi, err := summaryStatFn(indexPath)
			if err != nil {
				return sessionsIndex{}, err
			}
			mtime = fi.ModTime().UnixNano()
		}
		// Re-check inside the flight: an earlier flight may have just populated
		// the cache, so a burst does not re-read the file N times.
		if cachedIdx, ok := s.getCachedSummary(indexPath, mtime); ok {
			return cachedIdx, nil
		}
		data, err := os.ReadFile(indexPath)
		if err != nil {
			return sessionsIndex{}, err
		}
		var idx sessionsIndex
		if err := json.Unmarshal(data, &idx); err != nil {
			return sessionsIndex{}, err
		}
		s.setCachedSummary(indexPath, mtime, idx)
		return idx, nil
	})
	if err != nil {
		return sessionsIndex{}, false
	}
	return v.(sessionsIndex), true
}

// getCachedSummary checks the summary cache under RLock; a hit refreshes gen lock-free.
func (s *Scanner) getCachedSummary(indexPath string, mtime int64) (sessionsIndex, bool) {
	s.summaryCache.RLock()
	cached, ok := s.summaryCache.entries[indexPath]
	s.summaryCache.RUnlock()
	if !ok || cached.mtime != mtime {
		return sessionsIndex{}, false
	}
	// Refresh gen lock-free so eviction keeps the entry alive; the entry
	// address is stable so no write-lock upgrade is needed (#2330).
	cached.gen.Store(s.summaryCache.generation.Load())
	return cached.index, true
}

// setCachedSummary writes a summary cache entry under a deferred lock.
func (s *Scanner) setCachedSummary(indexPath string, mtime int64, idx sessionsIndex) {
	s.summaryCache.Lock()
	defer s.summaryCache.Unlock()
	entry := &summaryCacheEntry{mtime: mtime, index: idx}
	entry.gen.Store(s.summaryCache.generation.Load())
	s.summaryCache.entries[indexPath] = entry
	s.evictSummaryCache()
}

// RefreshDynamic updates the mutable fields (LastActive, State, Summary,
// LastPrompt) of already-discovered sessions in place using Scan's caches, so
// unchanged files cost an os.Stat + cache hit. Returns true if anything changed.
func RefreshDynamic(claudeDir string, sessions []DiscoveredSession) bool {
	return DefaultScanner().RefreshDynamic(claudeDir, sessions)
}

// RefreshDynamicContext is the cancellation-aware variant of RefreshDynamic;
// like ScanContext it guards the promptSem fan-out against a hung FS (#2244).
func RefreshDynamicContext(ctx context.Context, claudeDir string, sessions []DiscoveredSession) bool {
	return DefaultScanner().RefreshDynamicContext(ctx, claudeDir, sessions)
}

// RefreshDynamic deliberately does NOT advance the cache generations — Scan is
// the sole authority for aging. Advancing here too would double-tick gen when
// both run in one cycle, halving cache lifetime and re-parsing JSONLs.
func (s *Scanner) RefreshDynamic(claudeDir string, sessions []DiscoveredSession) bool {
	return s.RefreshDynamicContext(context.Background(), claudeDir, sessions)
}

// RefreshDynamicContext is the cancellation-aware variant of
// (*Scanner).RefreshDynamic; see the package-level RefreshDynamicContext.
func (s *Scanner) RefreshDynamicContext(ctx context.Context, claudeDir string, sessions []DiscoveredSession) bool {
	if claudeDir == "" || len(sessions) == 0 {
		return false
	}

	workspaces := make(map[string]string, len(sessions))
	for i := range sessions {
		workspaces[sessions[i].SessionID] = sessions[i].CWD
	}
	summaryMap := s.LookupSummaries(claudeDir, workspaces)

	// Batch-extract last prompts in parallel; the Stat the extraction performs
	// also yields the JSONL mtime, reused below instead of a second os.Stat.
	prompts := make([]string, len(sessions))
	mtimes := make([]int64, len(sessions))
	mtimeOK := make([]bool, len(sessions))
	var wg sync.WaitGroup
	sem := s.promptSem
	for i := range sessions {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Abandon not-yet-started extractions on ctx cancellation so a hung
			// FS cannot park this goroutine past shutdown; mtimeOK[idx] stays
			// false and the merge loop falls back to StartedAt (#2244).
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			prompts[idx], mtimes[idx], mtimeOK[idx] = s.extractLastPromptWithMtime(
				claudeDir, sessions[idx].CWD, sessions[idx].SessionID)
		}(i)
	}
	wg.Wait()

	changed := false
	nowMs := time.Now().UnixMilli()
	for i := range sessions {
		sess := &sessions[i]
		la := sess.StartedAt
		if mtimeOK[i] {
			la = mtimes[i]
		}
		if la != sess.LastActive {
			sess.LastActive = la
			changed = true
		}
		newState := "ready"
		if sess.LastActive > nowMs-int64(runningThreshold/time.Millisecond) {
			newState = "running"
		}
		if newState != sess.State {
			sess.State = newState
			changed = true
		}
		if sum := SanitizePromptForTransport(summaryMap[sess.SessionID]); sum != "" && sum != sess.Summary {
			sess.Summary = sum
			changed = true
		}
		if prompts[i] != sess.LastPrompt {
			sess.LastPrompt = prompts[i]
			changed = true
		}
	}
	return changed
}

// IsValidSessionID checks whether s is a UUID-format session ID (8-4-4-4-12
// lowercase hex). Hand-rolled to avoid the regexp DFA cost paid on every
// discovered session per Scan.
func IsValidSessionID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// WaitAndCleanup waits for pid to exit (up to 5 s or until ctx is cancelled),
// sends SIGKILL if still alive and PID identity matches, then removes stale
// session metadata and lock files. Must be called after SIGTERM has already been sent.
func WaitAndCleanup(ctx context.Context, pid int, procStartTime uint64, claudeDir, cwd, sessionID string) {
	ctxCancelled := waitForExit(ctx, pid)
	if !ctxCancelled && procStartTime != 0 {
		if actual, err := ProcStartTime(pid); err == nil && actual == procStartTime {
			procKillSIGKILL(pid)
		}
	}
	if claudeDir != "" {
		_ = os.Remove(filepath.Join(claudeDir, "sessions", fmt.Sprintf("%d.json", pid)))
	}
	if cwd != "" && sessionID != "" && IsValidSessionID(sessionID) {
		encodedCWD := projDirName(cwd)
		tmpBase := os.TempDir()
		lockDir := filepath.Clean(filepath.Join(tmpBase, fmt.Sprintf("claude-%d", os.Getuid()), encodedCWD, sessionID))
		// filepath.Rel verifies lockDir stays strictly beneath os.TempDir();
		// string-prefix matching is fragile against sibling names (/tmp vs
		// /tmp10) and a lockDir that collapses to tmpBase itself.
		if rel, err := filepath.Rel(tmpBase, lockDir); err == nil &&
			rel != "." && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			_ = os.RemoveAll(lockDir)
		}
	}
}

// waitForExit polls until the process exits or ctx is cancelled, returning
// true if ctx was cancelled first. A single timer is reused across the
// back-off loop to avoid per-iteration time.NewTimer allocations.
func waitForExit(ctx context.Context, pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	wait := 50 * time.Millisecond
	t := time.NewTimer(wait)
	defer t.Stop()
	for time.Now().Before(deadline) {
		if !procPidAlive(pid) {
			return false
		}
		select {
		case <-ctx.Done():
			return true
		case <-t.C:
		}
		if wait < 500*time.Millisecond {
			wait *= 2
		}
		t.Reset(wait)
	}
	return false
}
