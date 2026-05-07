package discovery

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/osutil"
)

// Scanner holds the mutable caches that used to be package-level globals
// (promptCache, summaryCache). Existing call sites hit the compat
// wrappers below — `Scan` / `LookupSummaries` / `RefreshDynamic` — which
// delegate to DefaultScanner(). Tests that need isolation (e.g. parallel
// subtests that would otherwise contend on the package globals) can
// instantiate a fresh `*Scanner` via NewScanner() and use its methods
// directly. Without this struct, `scanner_test.go` could not run any
// test with `t.Parallel()` — all 29 tests are serial today.
//
// Field semantics:
//
//	promptCache   — hit-result cache for extractLastPrompt, keyed by
//	                (JSONL path, mtime). Cleared by Scan's generation
//	                counter once it exceeds 500 entries.
//	summaryCache  — hit-result cache for LookupSummaries' index-file
//	                reads, same 500-entry bound.
//
// The two caches share semantics but hold different value types, so they
// do not unify into one field without an empty-interface box — kept
// separate for compile-time safety.
//
// pathCache accelerates findSessionJSONL's fallback-scan path: when cwd
// is unknown the scanner must os.ReadDir(claudeDir/projects) and Stat one
// `<project>/<sessionID>.jsonl` candidate per project. On fleets with
// hundreds of historical project directories that is O(N) syscalls per
// lookup — amplified by NewRouter's 10-wide historyWg goroutine pool
// replaying resume chains at startup. A small map keyed by
// (claudeDir + sessionID) collapses the repeat cost: positive hits
// re-validate with a single os.Stat, negative hits expire after a short
// TTL so a newly-created JSONL is picked up automatically.
type Scanner struct {
	promptCache  promptCacheState
	summaryCache summaryCacheState
	pathCache    pathCacheState
}

type promptCacheState struct {
	sync.RWMutex
	entries    map[string]promptCacheEntry
	generation uint64
}

type promptCacheEntry struct {
	mtime  int64
	prompt string
	gen    uint64
}

type summaryCacheState struct {
	sync.RWMutex
	entries    map[string]summaryCacheEntry
	generation uint64
}

type summaryCacheEntry struct {
	mtime int64
	index sessionsIndex
	gen   uint64
}

// pathCacheState maps (claudeDir + "\x00" + sessionID) to the resolved
// JSONL path when known, or a zero-path entry with a negativeUntil
// deadline when a recent scan failed to locate the file. Access is
// arbitrated by sync.RWMutex: the hit path uses RLock and the slow
// `os.ReadDir` + Stat fan-out takes the write lock only to commit
// results. findSessionJSONL can race with itself for distinct
// sessionIDs without contention because the map lookup is O(1).
type pathCacheState struct {
	sync.RWMutex
	entries map[string]pathCacheEntry
}

// pathCacheEntry holds either a positive result (path != "") or a
// bounded negative result (path == "" && !negativeUntil.IsZero()).
// Positive entries have no explicit TTL: callers validate the path
// with os.Stat and drop the cache on mismatch, so claude CLI deleting
// or renaming the JSONL self-heals on the next lookup. Negative
// entries expire after pathCacheNegativeTTL so a legitimately new
// JSONL (e.g. a session that started after the last scan) eventually
// makes it past the cache rather than being shadowed forever.
type pathCacheEntry struct {
	path          string
	negativeUntil time.Time
}

// pathCacheNegativeTTL caps how long a "scanned everything and didn't
// find it" verdict stays cached. 60s matches the feeling of "retry
// soon if you care" without letting a startup burst of 10 concurrent
// resume-chain walks each pay for a full os.ReadDir pass. Positive
// entries have no TTL — os.Stat revalidation covers invalidation.
const pathCacheNegativeTTL = 60 * time.Second

// pathCacheMaxEntries bounds the map so a long-running process that
// sees tens of thousands of distinct sessionIDs (via resume chains or
// dashboard queries) does not grow the map without limit. When the
// cap is reached expired negative entries are dropped first; if all
// entries are positive or fresh-negative, evictPathCacheLocked falls
// through to arbitrary (random map-iteration) eviction so the cap is
// enforced unconditionally.
const pathCacheMaxEntries = 2048

// pathCacheEvictBatch is the headroom evictPathCacheLocked creates after
// running the arbitrary-eviction fallback pass. Dropping exactly one entry
// per store at the cap would thrash the map — this cushions the cap so
// subsequent stores amortise the eviction pass.
const pathCacheEvictBatch = 16

// NewScanner returns a fresh Scanner with empty caches. Used directly by
// tests that need isolation; production callers use the package-level
// wrappers which hit DefaultScanner.
func NewScanner() *Scanner {
	return &Scanner{
		promptCache:  promptCacheState{entries: make(map[string]promptCacheEntry)},
		summaryCache: summaryCacheState{entries: make(map[string]summaryCacheEntry)},
		pathCache:    pathCacheState{entries: make(map[string]pathCacheEntry)},
	}
}

// DefaultScanner returns the process-wide Scanner used by the package-
// level wrappers. Lazy-initialized via sync.Once so callers that never
// invoke Scan/LookupSummaries don't allocate the cache maps.
var (
	defaultScannerOnce sync.Once
	defaultScannerInst *Scanner
)

func DefaultScanner() *Scanner {
	defaultScannerOnce.Do(func() {
		defaultScannerInst = NewScanner()
	})
	return defaultScannerInst
}

// evictPromptCache deletes entries that are more than one generation old.
// Eviction only runs when the cache exceeds 500 entries.
// Must be called with s.promptCache.Lock() held.
func (s *Scanner) evictPromptCache() {
	if len(s.promptCache.entries) <= 500 {
		return
	}
	for k, v := range s.promptCache.entries {
		if v.gen+1 < s.promptCache.generation {
			delete(s.promptCache.entries, k)
		}
	}
}

// evictSummaryCache deletes entries that are more than one generation old.
// Eviction only runs when the cache exceeds 500 entries.
// Must be called with s.summaryCache.Lock() held.
func (s *Scanner) evictSummaryCache() {
	if len(s.summaryCache.entries) <= 500 {
		return
	}
	for k, v := range s.summaryCache.entries {
		if v.gen+1 < s.summaryCache.generation {
			delete(s.summaryCache.entries, k)
		}
	}
}

// runningThreshold is the JSONL mtime recency window used to classify a
// discovered process as "running" (actively writing) vs "ready" (idle).
// Set to 30s to avoid status flapping: Claude CLI may write JSONL during
// idle housekeeping (compaction, MCP events, session index updates), so a
// narrow window (e.g. 5s) causes ready->running oscillation on every scan.
const runningThreshold = 30 * time.Second

// MaxSafeJSONInt mirrors JavaScript's Number.MAX_SAFE_INTEGER (2^53 - 1).
// Any uint64 field that crosses a JSON boundary and may be consumed by a JS
// front-end (dashboard.js) or a reverse-RPC peer that proxies values into
// JSON must stay below this ceiling, otherwise JSON.parse silently truncates
// to the nearest double and PID-identity comparisons (ProcStartTime) return
// wrong matches after the rounding.
//
// Current producers stay well below the bound:
//   - Linux ProcStartTime (jiffies since boot @ 100 Hz): reaching 2^53 needs
//     ~2.85 million years of uptime.
//   - Darwin ProcStartTime (Unix microseconds): reaching 2^53 needs ~2255 CE
//     (present Unix μs ≈ 1.77e15 ≪ 9.00e15).
//
// The constant exists to pin the invariant as a source-level contract: the
// proc_{linux,darwin}_test.go suites assert ProcStartTime(os.Getpid()) <=
// MaxSafeJSONInt so a future encoding change (e.g. nanoseconds, or a non-
// epoch reference) that silently blows the budget fails at CI time.
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
	// ProcStartTime encodes a per-PID boot identity used to detect PID reuse.
	// Linux:  /proc/PID/stat field 22 (jiffies since system boot).
	// Darwin: Unix microseconds parsed from `ps -o lstart=`.
	// Crosses JSON boundaries into dashboard.js and reverse-RPC payloads —
	// MUST stay below MaxSafeJSONInt (2^53-1), otherwise JS JSON.parse
	// truncates the value and handleTakeover's identity equality fails.
	// Both producer paths remain bounded by physical units; see MaxSafeJSONInt.
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

// Scan is the package-level wrapper that delegates to DefaultScanner.
// Preserves the pre-refactor signature so call sites in server/ and cmd/
// do not need to change. Use (*Scanner).Scan directly when you need cache
// isolation (e.g. parallel test subtests).
func Scan(claudeDir string, excludePIDs map[int]bool, excludeSessionIDs map[string]bool, managedCWDs map[string]bool) ([]DiscoveredSession, error) {
	return DefaultScanner().Scan(claudeDir, excludePIDs, excludeSessionIDs, managedCWDs)
}

// Scan reads ~/.claude/sessions/*.json and returns live Claude CLI processes
// that are not managed by naozhi (excluded via excludePIDs).
// excludeSessionIDs prevents the session-ID upgrade heuristic from assigning
// a JSONL file that belongs to a naozhi-managed session to a CLI process.
// managedCWDs is the set of working directories that have active managed sessions;
// session ID upgrade is skipped entirely for these CWDs to prevent cross-contamination.
func (s *Scanner) Scan(claudeDir string, excludePIDs map[int]bool, excludeSessionIDs map[string]bool, managedCWDs map[string]bool) ([]DiscoveredSession, error) {
	sessDir := filepath.Join(claudeDir, "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	// Advance cache generations once per scan so the eviction logic can
	// identify entries that have not been touched in the last two scan cycles.
	s.promptCache.Lock()
	s.promptCache.generation++
	s.promptCache.Unlock()

	s.summaryCache.Lock()
	s.summaryCache.generation++
	s.summaryCache.Unlock()

	// First pass: collect alive sessions with their original session IDs.
	var candidates []scanCandidate

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(sessDir, entry.Name()))
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

		la := jsonlMtime(claudeDir, sf.CWD, sf.SessionID, sf.StartedAt)
		candidates = append(candidates, scanCandidate{sf: sf, lastActive: la})
	}

	// Second pass: upgrade stale session IDs. CLI doesn't update {pid}.json
	// after /clear, so the session ID may be outdated. For each CWD, find
	// recent JSONL files and assign them to PIDs.
	// Strategy: sort PIDs by original staleness (most stale first = most
	// likely to have done /clear), assign newest unassigned JSONL to each.
	type cwdGroup struct {
		indices []int // indices into candidates
	}
	groups := map[string]*cwdGroup{}
	for i, c := range candidates {
		g, ok := groups[c.sf.CWD]
		if !ok {
			g = &cwdGroup{}
			groups[c.sf.CWD] = g
		}
		g.indices = append(g.indices, i)
	}

	for cwd, g := range groups {
		// Skip session ID upgrade when multiple processes share the same CWD.
		// The heuristic is non-deterministic with multiple live processes and
		// can swap session IDs between scans, causing takeover to target the
		// wrong process.
		if len(g.indices) > 1 {
			continue
		}

		// Skip upgrade when a managed naozhi session is using the same CWD.
		// Any recent JSONL in this directory likely belongs to the managed
		// session, not the CLI process.
		if managedCWDs[cwd] {
			continue
		}

		recentJSONLs := listJSONLsByMtime(claudeDir, cwd)
		if len(recentJSONLs) == 0 {
			continue
		}

		// Build set of "claimed" session IDs (original assignments).
		// Pre-claim managed naozhi session IDs so they are never assigned to
		// a CLI process — the same workspace can have both a CLI and a managed
		// session writing JSONL files to the same project directory.
		claimed := map[string]bool{}
		for id := range excludeSessionIDs {
			claimed[id] = true
		}
		for _, idx := range g.indices {
			claimed[candidates[idx].sf.SessionID] = true
		}

		// Sort group indices by staleness (most stale first)
		sortByLastActive(g.indices, candidates)

		for _, idx := range g.indices {
			c := &candidates[idx]
			// Find newest unclaimed JSONL newer than this PID's current session
			for _, jl := range recentJSONLs {
				if claimed[jl.id] {
					continue
				}
				if jl.mtime > c.lastActive {
					c.sf.SessionID = jl.id // will be used below
					c.lastActive = jl.mtime
					claimed[jl.id] = true
					break
				}
			}
		}
	}

	// Batch-lookup summaries from sessions-index.json for all candidates.
	candidateWorkspaces := make(map[string]string, len(candidates))
	for i := range candidates {
		candidateWorkspaces[candidates[i].sf.SessionID] = candidates[i].sf.CWD
	}
	summaryMap := s.LookupSummaries(claudeDir, candidateWorkspaces)

	// Batch-extract last prompts in parallel (up to 4 concurrent I/O operations)
	// to avoid serial 512KB reads per discovered session.
	prompts := make([]string, len(candidates))
	var promptWg sync.WaitGroup
	promptSem := make(chan struct{}, 4)
	for i := range candidates {
		promptWg.Add(1)
		go func(idx int) {
			defer promptWg.Done()
			promptSem <- struct{}{}
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
			Summary:       summaryMap[c.sf.SessionID],
			LastPrompt:    prompts[i],
			ProcStartTime: pst,
		})
	}
	return result, nil
}

// processAlive checks whether a process with the given PID exists.
// Delegates to osutil.PidAlive so the pid<=0 guard (kill(0, sig) broadcasts
// to the whole process group and kill(-N, sig) targets groups — both would
// misreport phantom processes as alive) is consistent across packages.
func processAlive(pid int) bool {
	return osutil.PidAlive(pid)
}

type jsonlEntry struct {
	id    string // session UUID (filename without .jsonl)
	mtime int64  // unix ms
}

// listJSONLsByMtime returns JSONL files in the project dir sorted by mtime desc.
func listJSONLsByMtime(claudeDir, cwd string) []jsonlEntry {
	projDir := filepath.Join(claudeDir, "projects", projDirName(cwd))
	entries, err := os.ReadDir(projDir)
	if err != nil {
		return nil
	}

	var result []jsonlEntry
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		result = append(result, jsonlEntry{
			id:    strings.TrimSuffix(name, ".jsonl"),
			mtime: info.ModTime().UnixMilli(),
		})
	}

	slices.SortFunc(result, func(a, b jsonlEntry) int {
		return cmp.Compare(b.mtime, a.mtime) // newest first
	})
	return result
}

// sortByLastActive sorts candidate indices by lastActive ascending (most stale first).
func sortByLastActive(indices []int, candidates []scanCandidate) {
	slices.SortFunc(indices, func(a, b int) int {
		return cmp.Compare(candidates[a].lastActive, candidates[b].lastActive)
	})
}

// projDirName converts a CWD path to the Claude project directory name.
// e.g. "/home/user/workspace/foo" -> "-home-user-workspace-foo"
func projDirName(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
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
	path := findJSONLPath(claudeDir, cwd, sessionID)
	if path == "" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mtime := fi.ModTime().UnixNano()

	if cached, ok := s.getCachedPrompt(path, mtime); ok {
		return cached
	}

	result := extractLastPromptUncached(path, fi.Size())

	s.setCachedPrompt(path, mtime, result)
	return result
}

// getCachedPrompt checks the prompt cache. Reads use RLock; only the gen
// refresh on a hit upgrades to the write lock (cheap in-place update).
func (s *Scanner) getCachedPrompt(path string, mtime int64) (string, bool) {
	s.promptCache.RLock()
	cached, ok := s.promptCache.entries[path]
	gen := s.promptCache.generation
	s.promptCache.RUnlock()
	if !ok || cached.mtime != mtime {
		return "", false
	}
	if cached.gen != gen {
		s.promptCache.Lock()
		if e, ok2 := s.promptCache.entries[path]; ok2 && e.mtime == mtime {
			e.gen = s.promptCache.generation
			s.promptCache.entries[path] = e
		}
		s.promptCache.Unlock()
	}
	return cached.prompt, true
}

// setCachedPrompt writes a prompt cache entry under a deferred lock.
func (s *Scanner) setCachedPrompt(path string, mtime int64, result string) {
	s.promptCache.Lock()
	defer s.promptCache.Unlock()
	s.promptCache.entries[path] = promptCacheEntry{mtime: mtime, prompt: result, gen: s.promptCache.generation}
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

	// Read up to the last 512KB of the file
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

	// If the tail scan found no text prompt and we skipped earlier content,
	// re-scan from the beginning. This handles sessions where the only user
	// text prompt is near the start and the tail is all tool_result messages.
	if lastPrompt == "" && offset > 0 {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			lastPrompt = scanUserPrompt(f)
		}
	}

	return cli.TruncateRunes(lastPrompt, 120)
}

// scanUserPrompt scans lines from the current file position and returns
// the last user message that contains actual text (not tool_result, and
// not one of Claude Code's system-injected XML frames).
func scanUserPrompt(f *os.File) string {
	var lastPrompt string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 16*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Quick check before full parse
		if !bytes.Contains(line, []byte(`"type":"user"`)) {
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
	return lastPrompt
}

// claudeSystemInjectedTagNames enumerates the XML-like tags that Claude
// Code and its plugins inject as synthetic user messages (task queue
// notifications, hook system reminders, slash-command envelopes, deferred-
// tool announcements). These are operational noise, not user intent, and
// must not become the session title or an entry in the history view.
//
// Kept in lockstep with the UI-side filter in internal/server/static/dashboard.js
// (eventHtml + formatSessionMarkdown). When adding a tag in one place, add
// it in the other, otherwise exports and titles drift apart.
var claudeSystemInjectedTagNames = [...]string{
	"task-notification",
	"system-reminder",
	"local-command",
	"command-name",
	"available-deferred-tools",
}

// IsClaudeSystemInjectedText reports whether text is a Claude-Code-injected
// system XML frame (e.g. "<task-notification>…"). A leading "<tag>" or
// "<tag " counts as a match; anything else is treated as real user content.
func IsClaudeSystemInjectedText(text string) bool {
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

// extractUserText extracts the text content from a user message.
func extractUserText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil || len(msg.Content) == 0 {
		return ""
	}
	// Try string
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return strings.TrimSpace(s)
	}
	// Try []block
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

// ProcStartTime and detectCLIName are in platform-specific files:
//   proc_linux.go  — reads /proc/PID/stat and /proc/PID/cmdline
//   proc_darwin.go — uses sysctl and ps(1)

var sessionIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// LookupSummaries is the package-level wrapper that delegates to
// DefaultScanner. Preserves the pre-refactor signature for zero-churn
// back-compat at call sites.
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
	byProjDir := make(map[string][]string) // indexPath → []sessionID
	for sid, workspace := range sessions {
		if workspace == "" {
			continue
		}
		indexPath := filepath.Join(claudeDir, "projects", projDirName(workspace), "sessions-index.json")
		byProjDir[indexPath] = append(byProjDir[indexPath], sid)
	}

	result := make(map[string]string, len(sessions))
	for indexPath, sids := range byProjDir {
		// Check mtime cache to avoid re-reading unchanged index files.
		fi, err := os.Stat(indexPath)
		if err != nil {
			continue
		}
		mtime := fi.ModTime().UnixNano()

		var idx sessionsIndex
		if cachedIdx, ok := s.getCachedSummary(indexPath, mtime); ok {
			idx = cachedIdx
		} else {
			data, err := os.ReadFile(indexPath)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(data, &idx); err != nil {
				continue
			}
			s.setCachedSummary(indexPath, mtime, idx)
		}

		// Build a lookup set once per project: large project directories may
		// have 100s of entries and multi-concurrent sids, so O(entries×sids)
		// scaling hurts. Map build + O(1) membership is faster once sids > ~3.
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
	return result
}

// getCachedSummary checks the summary cache. Reads use RLock; only the gen
// refresh on a hit upgrades to the write lock (cheap in-place update).
func (s *Scanner) getCachedSummary(indexPath string, mtime int64) (sessionsIndex, bool) {
	s.summaryCache.RLock()
	cached, ok := s.summaryCache.entries[indexPath]
	gen := s.summaryCache.generation
	s.summaryCache.RUnlock()
	if !ok || cached.mtime != mtime {
		return sessionsIndex{}, false
	}
	if cached.gen != gen {
		s.summaryCache.Lock()
		if e, ok2 := s.summaryCache.entries[indexPath]; ok2 && e.mtime == mtime {
			e.gen = s.summaryCache.generation
			s.summaryCache.entries[indexPath] = e
		}
		s.summaryCache.Unlock()
	}
	return cached.index, true
}

// setCachedSummary writes a summary cache entry under a deferred lock.
func (s *Scanner) setCachedSummary(indexPath string, mtime int64, idx sessionsIndex) {
	s.summaryCache.Lock()
	defer s.summaryCache.Unlock()
	s.summaryCache.entries[indexPath] = summaryCacheEntry{mtime: mtime, index: idx, gen: s.summaryCache.generation}
	s.evictSummaryCache()
}

// RefreshDynamic updates the mutable fields (LastActive, State, Summary,
// LastPrompt) of already-discovered sessions in place.  It uses the same
// caches as Scan, so repeated calls for unchanged JSONL/index files are cheap
// (os.Stat + cache hit).  Returns true if any field changed.
//
// RefreshDynamic is the package-level wrapper that delegates to
// DefaultScanner. Preserves the pre-refactor signature.
func RefreshDynamic(claudeDir string, sessions []DiscoveredSession) bool {
	return DefaultScanner().RefreshDynamic(claudeDir, sessions)
}

// RefreshDynamic deliberately does NOT advance promptCache/summaryCache
// generations — Scan is the sole authority for aging. Advancing here would
// double-tick gen when Scan and RefreshDynamic run in the same cycle,
// halving the effective cache lifetime (entries evicted after 1 cycle
// instead of 2) and triggering repeated JSONL parses.
func (s *Scanner) RefreshDynamic(claudeDir string, sessions []DiscoveredSession) bool {
	if claudeDir == "" || len(sessions) == 0 {
		return false
	}

	// Batch-lookup summaries.
	workspaces := make(map[string]string, len(sessions))
	for i := range sessions {
		workspaces[sessions[i].SessionID] = sessions[i].CWD
	}
	summaryMap := s.LookupSummaries(claudeDir, workspaces)

	// Batch-extract last prompts in parallel.
	prompts := make([]string, len(sessions))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range sessions {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			prompts[idx] = s.extractLastPrompt(claudeDir, sessions[idx].CWD, sessions[idx].SessionID)
		}(i)
	}
	wg.Wait()

	changed := false
	nowMs := time.Now().UnixMilli()
	for i := range sessions {
		sess := &sessions[i]
		if la := jsonlMtime(claudeDir, sess.CWD, sess.SessionID, sess.StartedAt); la != sess.LastActive {
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
		if sum := summaryMap[sess.SessionID]; sum != "" && sum != sess.Summary {
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

// IsValidSessionID checks whether s is a valid UUID-format session ID.
func IsValidSessionID(s string) bool {
	return sessionIDRe.MatchString(s)
}

// WaitAndCleanup waits for pid to exit (up to 5 s or until ctx is cancelled),
// sends SIGKILL if still alive and PID identity matches, then removes stale
// session metadata and lock files. Must be called after SIGTERM has already been sent.
func WaitAndCleanup(ctx context.Context, pid int, procStartTime uint64, claudeDir, cwd, sessionID string) {
	ctxCancelled := waitForExit(ctx, pid)
	if !ctxCancelled && procStartTime != 0 {
		if actual, err := ProcStartTime(pid); err == nil && actual == procStartTime {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if claudeDir != "" {
		_ = os.Remove(filepath.Join(claudeDir, "sessions", fmt.Sprintf("%d.json", pid)))
	}
	if cwd != "" && sessionID != "" && IsValidSessionID(sessionID) {
		encodedCWD := strings.ReplaceAll(cwd, "/", "-")
		tmpBase := os.TempDir()
		lockDir := filepath.Clean(filepath.Join(tmpBase, fmt.Sprintf("claude-%d", os.Getuid()), encodedCWD, sessionID))
		// Defense-in-depth: use filepath.Rel to verify lockDir stays
		// strictly beneath os.TempDir(). String-prefix matching is fragile
		// against sibling names (e.g. /tmp vs /tmp10 where the cleaned
		// paths happen to share a prefix byte sequence) and against a
		// lockDir that collapses to tmpBase itself.
		if rel, err := filepath.Rel(tmpBase, lockDir); err == nil &&
			rel != "." && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			_ = os.RemoveAll(lockDir)
		}
	}
}

// waitForExit polls until the process exits or ctx is cancelled.
// Returns true if ctx was cancelled before the process exited.
func waitForExit(ctx context.Context, pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	wait := 50 * time.Millisecond
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return false
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return true
		case <-t.C:
		}
		if wait < 500*time.Millisecond {
			wait *= 2
		}
	}
	return false
}
