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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// runningThreshold is the JSONL mtime recency window used to classify a
// discovered process as "running" (actively writing) vs "ready" (idle).
const runningThreshold = 5 * time.Second

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
func Scan(claudeDir string, excludePIDs map[int]bool) ([]DiscoveredSession, error) {
	sessDir := filepath.Join(claudeDir, "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

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

		recentJSONLs := listJSONLsByMtime(claudeDir, cwd)
		if len(recentJSONLs) == 0 {
			continue
		}

		// Build set of "claimed" session IDs (original assignments)
		claimed := map[string]bool{}
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

	// Cache parsed sessions-index.json per project directory to avoid
	// re-reading the same file for every session sharing a CWD.
	indexCache := make(map[string]*sessionsIndex)
	cachedLookupSummary := func(cwd, sessionID string) string {
		indexPath := filepath.Join(claudeDir, "projects", projDirName(cwd), "sessions-index.json")
		idx, ok := indexCache[indexPath]
		if !ok {
			data, err := os.ReadFile(indexPath)
			if err != nil {
				indexCache[indexPath] = nil
				return ""
			}
			var parsed sessionsIndex
			if err := json.Unmarshal(data, &parsed); err != nil {
				indexCache[indexPath] = nil
				return ""
			}
			idx = &parsed
			indexCache[indexPath] = idx
		}
		if idx == nil {
			return ""
		}
		for _, e := range idx.Entries {
			if e.SessionID == sessionID {
				return e.Summary
			}
		}
		return ""
	}

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
			Summary:       cachedLookupSummary(c.sf.CWD, c.sf.SessionID),
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
	Entries []sessionsIndexEntry `json:"entries"`
}

type sessionsIndexEntry struct {
	SessionID   string `json:"sessionId"`
	Summary     string `json:"summary"`
	FirstPrompt string `json:"firstPrompt"`
}

// extractLastPrompt reads the JSONL file backwards to find the last user message.
// Only reads up to 512KB from the tail to keep Scan fast.
func extractLastPrompt(claudeDir, cwd, sessionID string) string {
	path := findJSONLPath(claudeDir, cwd, sessionID)
	if path == "" {
		return ""
	}

	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Read up to the last 512KB of the file
	const tailSize = 512 * 1024
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := fi.Size() - tailSize
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
