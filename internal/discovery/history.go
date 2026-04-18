package discovery

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// historyLine is the minimal schema for a ~/.claude/projects/.../{sessionId}.jsonl line.
type historyLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"` // RFC3339
	Message   json.RawMessage `json:"message"`
}

type historyMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []block
}

type historyBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`  // tool_use
	Input json.RawMessage `json:"input"` // tool_use
}

// LoadHistory finds and parses the JSONL for sessionID under claudeDir/projects/,
// returning EventEntries for user and assistant messages.
// If cwd is provided, the JSONL is located directly via the CWD-based path (O(1)).
// Otherwise falls back to scanning all project directories.
func LoadHistory(claudeDir, sessionID string, cwd ...string) ([]cli.EventEntry, error) {
	var path string
	if len(cwd) > 0 && cwd[0] != "" {
		candidate := filepath.Join(claudeDir, "projects", projDirName(cwd[0]), sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
		}
	}
	if path == "" {
		var err error
		path, err = findSessionJSONL(claudeDir, sessionID)
		if err != nil || path == "" {
			return nil, err
		}
	}
	return parseJSONL(path)
}

// findSessionJSONL searches claudeDir/projects/**/{sessionID}.jsonl.
func findSessionJSONL(claudeDir, sessionID string) (string, error) {
	projectsDir := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read projects dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, e.Name(), sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", nil
}

func parseJSONL(path string) ([]cli.EventEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entries := make([]cli.EventEntry, 0, 128)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var hl historyLine
		if err := json.Unmarshal(line, &hl); err != nil {
			slog.Debug("skip malformed history line", "path", path, "err", err)
			continue
		}

		ts := parseTimestamp(hl.Timestamp)

		switch hl.Type {
		case "user":
			var msg historyMessage
			if err := json.Unmarshal(hl.Message, &msg); err != nil {
				continue
			}
			text := extractText(msg.Content)
			if text == "" {
				continue
			}
			entries = append(entries, cli.EventEntry{
				Time:    ts,
				Type:    "user",
				Summary: cli.TruncateRunes(text, 120),
				Detail:  cli.TruncateRunes(text, 2000),
			})

		case "assistant":
			var msg historyMessage
			if err := json.Unmarshal(hl.Message, &msg); err != nil {
				continue
			}
			var blocks []historyBlock
			if err := json.Unmarshal(msg.Content, &blocks); err != nil {
				continue
			}
			for _, b := range blocks {
				// Only parse text blocks for history display.
				// tool_use and thinking are always filtered out by the
				// dashboard (eventHtml / processEventsForDisplay) and
				// would waste slots in the 500-entry persistedHistory
				// budget, pushing visible user/text entries out.
				if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
					continue
				}
				entries = append(entries, cli.EventEntry{
					Time:    ts,
					Type:    "text",
					Summary: cli.TruncateRunes(b.Text, 120),
					Detail:  cli.TruncateRunes(b.Text, 16000),
				})
			}
		}
	}
	return entries, scanner.Err()
}

// extractText handles content that is either a plain string or []block.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of blocks
	var blocks []historyBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func parseTimestamp(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}
