package discovery

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

	nowMs := time.Now().UnixMilli()
	var result []DiscoveredSession
	for i := range candidates {
		c := &candidates[i]
		pst, _ := ProcStartTime(c.sf.PID)
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
			LastPrompt:    extractLastPrompt(claudeDir, c.sf.CWD, c.sf.SessionID),
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
	return proc.Signal(syscall.Signal(0)) == nil
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

	sort.Slice(result, func(i, j int) bool {
		return result[i].mtime > result[j].mtime // newest first
	})
	return result
}

// sortByLastActive sorts candidate indices by lastActive ascending (most stale first).
func sortByLastActive(indices []int, candidates []scanCandidate) {
	sort.Slice(indices, func(i, j int) bool {
		return candidates[indices[i]].lastActive < candidates[indices[j]].lastActive
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

	promptCache.Lock()
	if cached, ok := promptCache.entries[path]; ok && cached.mtime == mtime {
		cached.gen = promptCache.generation
		promptCache.entries[path] = cached
		promptCache.Unlock()
		return cached.prompt
	}
	promptCache.Unlock()

	result := extractLastPromptUncached(path, fi.Size())

	promptCache.Lock()
	promptCache.entries[path] = promptCacheEntry{mtime: mtime, prompt: result, gen: promptCache.generation}
	evictPromptCache()
	promptCache.Unlock()
	return result
}

// extractLastPromptUncached does the actual 512KB tail read and JSON scanning.
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

	return cli.TruncateRunes(lastPrompt, 120)
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

// ProcStartTime reads the start time (field 22) from /proc/{pid}/stat.
// This value uniquely identifies a process instance even after PID reuse.
// Field 2 (comm) may contain spaces/parentheses, so we locate the last ')' first.
func ProcStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// Find the end of the comm field (last ')') to avoid parsing issues
	// with process names that contain spaces or parentheses.
	idx := bytes.LastIndexByte(data, ')')
	if idx < 0 || idx+2 >= len(data) {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	// Fields after comm start at index 3 (1-based). We need field 22 (starttime),
	// which is field index 22-3 = 19 in the remaining space-separated fields.
	fields := strings.Fields(string(data[idx+2:]))
	const startTimeIdx = 19 // 0-based index in fields after ')'
	if len(fields) <= startTimeIdx {
		return 0, fmt.Errorf("/proc/%d/stat: too few fields", pid)
	}
	return strconv.ParseUint(fields[startTimeIdx], 10, 64)
}

var sessionIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// detectCLIName reads /proc/PID/cmdline to determine which CLI binary is running.
// Returns "claude-code", "kiro", or "cli" as fallback.
func detectCLIName(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "cli"
	}
	// cmdline is NUL-separated; first field is the binary path.
	if i := bytes.IndexByte(data, 0); i >= 0 {
		data = data[:i]
	}
	bin := filepath.Base(string(data))
	switch {
	case strings.Contains(bin, "kiro"):
		return "kiro"
	case strings.Contains(bin, "claude"):
		return "claude-code"
	default:
		return "cli"
	}
}

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

		summaryCache.Lock()
		cached, ok := summaryCache.entries[indexPath]
		if ok && cached.mtime == mtime {
			cached.gen = summaryCache.generation
			summaryCache.entries[indexPath] = cached
			summaryCache.Unlock()
			wanted := make(map[string]bool, len(sids))
			for _, sid := range sids {
				wanted[sid] = true
			}
			for _, e := range cached.index.Entries {
				if wanted[e.SessionID] && e.Summary != "" {
					result[e.SessionID] = e.Summary
				}
			}
			continue
		}
		summaryCache.Unlock()

		data, err := os.ReadFile(indexPath)
		if err != nil {
			continue
		}
		var idx sessionsIndex
		if err := json.Unmarshal(data, &idx); err != nil {
			continue
		}

		summaryCache.Lock()
		summaryCache.entries[indexPath] = summaryCacheEntry{mtime: mtime, index: idx, gen: summaryCache.generation}
		evictSummaryCache()
		summaryCache.Unlock()

		wanted := make(map[string]bool, len(sids))
		for _, sid := range sids {
			wanted[sid] = true
		}
		for _, e := range idx.Entries {
			if wanted[e.SessionID] && e.Summary != "" {
				result[e.SessionID] = e.Summary
			}
		}
	}
	return result
}

// IsValidSessionID checks whether s is a valid UUID-format session ID.
func IsValidSessionID(s string) bool {
	return sessionIDRe.MatchString(s)
}

// WaitAndCleanup waits for pid to exit (up to 5 s), sends SIGKILL if still alive
// and PID identity matches, then removes stale session metadata and lock files.
// Must be called after SIGTERM has already been sent.
func WaitAndCleanup(pid int, procStartTime uint64, claudeDir, cwd, sessionID string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if procStartTime != 0 {
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
