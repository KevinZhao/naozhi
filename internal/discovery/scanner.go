package discovery

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/naozhi/naozhi/internal/cli"
)

// DiscoveredSession represents a Claude CLI process found on the system.
type DiscoveredSession struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"session_id"`
	CWD        string `json:"cwd"`
	StartedAt  int64  `json:"started_at"`            // unix ms
	LastActive int64  `json:"last_active"`           // unix ms (from JSONL mtime, fallback to started_at)
	Kind       string `json:"kind"`                  // "interactive" etc.
	Entrypoint string `json:"entrypoint"`            // "cli" etc.
	Summary    string `json:"summary,omitempty"`     // Claude-generated session name from sessions-index
	LastPrompt string `json:"last_prompt,omitempty"` // most recent user message
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

	var result []DiscoveredSession
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

		// Skip naozhi-managed processes
		if excludePIDs[sf.PID] {
			continue
		}

		// Check if process is alive
		if !processAlive(sf.PID) {
			continue
		}

		result = append(result, DiscoveredSession{
			PID:        sf.PID,
			SessionID:  sf.SessionID,
			CWD:        sf.CWD,
			StartedAt:  sf.StartedAt,
			LastActive: jsonlMtime(claudeDir, sf.CWD, sf.SessionID, sf.StartedAt),
			Kind:       sf.Kind,
			Entrypoint: sf.Entrypoint,
			Summary:    lookupSummary(claudeDir, sf.CWD, sf.SessionID),
			LastPrompt: extractLastPrompt(claudeDir, sf.CWD, sf.SessionID),
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

// lookupSummary reads the sessions-index.json for the project and returns the
// Claude-generated summary for the given session ID.
func lookupSummary(claudeDir, cwd, sessionID string) string {
	indexPath := filepath.Join(claudeDir, "projects", projDirName(cwd), "sessions-index.json")

	data, err := os.ReadFile(indexPath)
	if err != nil {
		return ""
	}
	var idx sessionsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return ""
	}
	for _, e := range idx.Entries {
		if e.SessionID == sessionID {
			return e.Summary
		}
	}
	return ""
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
		f.Seek(offset, io.SeekStart)
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
