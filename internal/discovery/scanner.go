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
)

// promptCache caches extractLastPrompt results keyed by (path, mtime).
// Avoids re-reading up to 512KB per discovered session on every 10s scan cycle.
var promptCache struct {
	sync.Mutex
	entries    map[string]promptCacheEntry
	generation uint64
}

type promptCacheEntry struct {
	mtime  int64
	prompt string
	gen    uint64
}

func init() {
	promptCache.entries = make(map[string]promptCacheEntry)
}

// summaryCache caches LookupSummaries index file reads.
var summaryCache struct {
	sync.Mutex
	entries    map[string]summaryCacheEntry
	generation uint64
}

type summaryCacheEntry struct {
	mtime int64
	index sessionsIndex
	gen   uint64
}

func init() {
	summaryCache.entries = make(map[string]summaryCacheEntry)
}

// evictPromptCache deletes entries that are more than one generation old.
// Eviction only runs when the cache exceeds 500 entries.
// Must be called with promptCache.Lock() held.
func evictPromptCache() {
	if len(promptCache.entries) <= 500 {
		return
	}
	for k, v := range promptCache.entries {
		if v.gen+1 < promptCache.generation {
			delete(promptCache.entries, k)
		}
	}
}

// evictSummaryCache deletes entries that are more than one generation old.
// Eviction only runs when the cache exceeds 500 entries.
// Must be called with summaryCache.Lock() held.
func evictSummaryCache() {
	if len(summaryCache.entries) <= 500 {
		return
	}
	for k, v := range summaryCache.entries {
		if v.gen+1 < summaryCache.generation {
			delete(summaryCache.entries, k)
		}
	}
}

// runningThreshold is the JSONL mtime recency window used to classify a
// discovered process as "running" (actively writing) vs "ready" (idle).
// Set to 30s to avoid status flapping: Claude CLI may write JSONL during
// idle housekeeping (compaction, MCP events, session index updates), so a
// narrow window (e.g. 5s) causes ready->running oscillation on every scan.
const runningThreshold = 30 * time.Second

// DiscoveredSession represents a Claude CLI process found on the system.
type DiscoveredSession struct {
	PID           int    `json:"pid"`
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	StartedAt     int64  `json:"started_at"`            // unix ms
	LastActive    int64  `json:"last_active"`           // unix ms (from JSONL mtime, fallback to started_at)
	State         string `json:"state"`                 // "running" or "ready"
	Kind          string `json:"kind"`                  // "interactive" etc.
	Entrypoint    string `json:"entrypoint"`            // "cli" etc.
	CLIName       string `json:"cli_name,omitempty"`    // "claude-code", "kiro" (detected from process cmdline)
	Summary       string `json:"summary,omitempty"`     // Claude-generated session name from sessions-index
	LastPrompt    string `json:"last_prompt,omitempty"` // most recent user message
	ProcStartTime uint64 `json:"proc_start_time"`       // /proc/PID/stat field 22, used to verify PID identity
	Project       string `json:"project,omitempty"`     // project name resolved from CWD (filled by server)
	Node          string `json:"node,omitempty"`        // workspace/node ID (filled by server for multi-node)
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

// Scan reads ~/.claude/sessions/*.json and returns live Claude CLI processes
// that are not managed by naozhi (excluded via excludePIDs).
// excludeSessionIDs prevents the session-ID upgrade heuristic from assigning
// a JSONL file that belongs to a naozhi-managed session to a CLI process.
// managedCWDs is the set of working directories that have active managed sessions;
// session ID upgrade is skipped entirely for these CWDs to prevent cross-contamination.
func Scan(claudeDir string, excludePIDs map[int]bool, excludeSessionIDs map[string]bool, managedCWDs map[string]bool) ([]DiscoveredSession, error) {
	sessDir := filepath.Join(claudeDir, "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Advance cache generations once per scan so the eviction logic can
	// identify entries that have not been touched in the last two scan cycles.
	promptCache.Lock()
	promptCache.generation++
	promptCache.Unlock()

	summaryCache.Lock()
	summaryCache.generation++
	summaryCache.Unlock()

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
	summaryMap := LookupSummaries(claudeDir, candidateWorkspaces)

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
			prompts[idx] = extractLastPrompt(claudeDir, candidates[idx].sf.CWD, candidates[idx].sf.SessionID)
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
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests existence without actually sending a signal.
	// EPERM means the process exists but is owned by a different user.
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
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
func extractLastPrompt(claudeDir, cwd, sessionID string) string {
	path := findJSONLPath(claudeDir, cwd, sessionID)
	if path == "" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mtime := fi.ModTime().UnixNano()

	if cached, ok := getCachedPrompt(path, mtime); ok {
		return cached
	}

	result := extractLastPromptUncached(path, fi.Size())

	setCachedPrompt(path, mtime, result)
	return result
}

// getCachedPrompt checks the prompt cache under a deferred lock.
func getCachedPrompt(path string, mtime int64) (string, bool) {
	promptCache.Lock()
	defer promptCache.Unlock()
	if cached, ok := promptCache.entries[path]; ok && cached.mtime == mtime {
		cached.gen = promptCache.generation
		promptCache.entries[path] = cached
		return cached.prompt, true
	}
	return "", false
}

// setCachedPrompt writes a prompt cache entry under a deferred lock.
func setCachedPrompt(path string, mtime int64, result string) {
	promptCache.Lock()
	defer promptCache.Unlock()
	promptCache.entries[path] = promptCacheEntry{mtime: mtime, prompt: result, gen: promptCache.generation}
	evictPromptCache()
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
// the last user message that contains actual text (not tool_result).
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
		if text != "" {
			lastPrompt = text
		}
	}
	return lastPrompt
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

// LookupSummaries looks up Claude-generated summaries for the given sessions.
// The sessions map is sessionID → workspace (CWD path).
// Returns a map of sessionID → summary.
func LookupSummaries(claudeDir string, sessions map[string]string) map[string]string {
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
		if cachedIdx, ok := getCachedSummary(indexPath, mtime); ok {
			idx = cachedIdx
		} else {
			data, err := os.ReadFile(indexPath)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(data, &idx); err != nil {
				continue
			}
			setCachedSummary(indexPath, mtime, idx)
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

// getCachedSummary checks the summary cache under a deferred lock.
func getCachedSummary(indexPath string, mtime int64) (sessionsIndex, bool) {
	summaryCache.Lock()
	defer summaryCache.Unlock()
	cached, ok := summaryCache.entries[indexPath]
	if ok && cached.mtime == mtime {
		cached.gen = summaryCache.generation
		summaryCache.entries[indexPath] = cached
		return cached.index, true
	}
	return sessionsIndex{}, false
}

// setCachedSummary writes a summary cache entry under a deferred lock.
func setCachedSummary(indexPath string, mtime int64, idx sessionsIndex) {
	summaryCache.Lock()
	defer summaryCache.Unlock()
	summaryCache.entries[indexPath] = summaryCacheEntry{mtime: mtime, index: idx, gen: summaryCache.generation}
	evictSummaryCache()
}

// RefreshDynamic updates the mutable fields (LastActive, State, Summary,
// LastPrompt) of already-discovered sessions in place.  It uses the same
// caches as Scan, so repeated calls for unchanged JSONL/index files are cheap
// (os.Stat + cache hit).  Returns true if any field changed.
//
// RefreshDynamic deliberately does NOT advance promptCache/summaryCache
// generations — Scan is the sole authority for aging. Advancing here would
// double-tick gen when Scan and RefreshDynamic run in the same cycle,
// halving the effective cache lifetime (entries evicted after 1 cycle
// instead of 2) and triggering repeated JSONL parses.
func RefreshDynamic(claudeDir string, sessions []DiscoveredSession) bool {
	if claudeDir == "" || len(sessions) == 0 {
		return false
	}

	// Batch-lookup summaries.
	workspaces := make(map[string]string, len(sessions))
	for i := range sessions {
		workspaces[sessions[i].SessionID] = sessions[i].CWD
	}
	summaryMap := LookupSummaries(claudeDir, workspaces)

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
			prompts[idx] = extractLastPrompt(claudeDir, sessions[idx].CWD, sessions[idx].SessionID)
		}(i)
	}
	wg.Wait()

	changed := false
	nowMs := time.Now().UnixMilli()
	for i := range sessions {
		s := &sessions[i]
		if la := jsonlMtime(claudeDir, s.CWD, s.SessionID, s.StartedAt); la != s.LastActive {
			s.LastActive = la
			changed = true
		}
		newState := "ready"
		if s.LastActive > nowMs-int64(runningThreshold/time.Millisecond) {
			newState = "running"
		}
		if newState != s.State {
			s.State = newState
			changed = true
		}
		if sum := summaryMap[s.SessionID]; sum != "" && sum != s.Summary {
			s.Summary = sum
			changed = true
		}
		if prompts[i] != s.LastPrompt {
			s.LastPrompt = prompts[i]
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
		lockDir := filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()), encodedCWD, sessionID)
		_ = os.RemoveAll(lockDir)
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
