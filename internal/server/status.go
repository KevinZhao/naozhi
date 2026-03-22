package server

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// formatEventLine converts a CLI event to a short status line for IM display.
// Returns empty string for events that don't warrant a status update.
func formatEventLine(ev cli.Event) string {
	if ev.Message == nil {
		return ""
	}
	for _, block := range ev.Message.Content {
		switch block.Type {
		case "thinking":
			if block.Text == "" {
				return ""
			}
			// Show first meaningful line of thinking, truncated
			first := firstLine(block.Text)
			return "💭 " + cli.TruncateRunes(first, 50)
		case "tool_use":
			return formatToolUse(block.Name, block.Input)
		}
	}
	return ""
}

func formatToolUse(name string, input json.RawMessage) string {
	switch name {
	case "Read":
		if p := extractParam(input, "file_path"); p != "" {
			return "📖 " + shortenPath(p)
		}
	case "Edit":
		if p := extractParam(input, "file_path"); p != "" {
			return "✏️ " + shortenPath(p)
		}
	case "Write":
		if p := extractParam(input, "file_path"); p != "" {
			return "📝 " + shortenPath(p)
		}
	case "Bash":
		if cmd := extractParam(input, "command"); cmd != "" {
			return "⚡ " + cli.TruncateRunes(cmd, 50)
		}
	case "Grep":
		if pat := extractParam(input, "pattern"); pat != "" {
			return "🔍 grep " + cli.TruncateRunes(pat, 40)
		}
	case "Glob":
		if pat := extractParam(input, "pattern"); pat != "" {
			return "🔍 " + cli.TruncateRunes(pat, 40)
		}
	case "Agent":
		if desc := extractParam(input, "description"); desc != "" {
			return "🤖 " + desc
		}
	}
	// Fallback: ACP tool_call titles or unknown tools
	return "🔧 " + name
}

func extractParam(input json.RawMessage, key string) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	val, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(val, &s); err != nil {
		return ""
	}
	return s
}

// shortenPath returns dir/base for readability.
func shortenPath(p string) string {
	dir := filepath.Base(filepath.Dir(p))
	base := filepath.Base(p)
	if dir == "." || dir == "/" {
		return base
	}
	return dir + "/" + base
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.SplitN(s, "\n", 3) {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return s
}

// statusAccumulator tracks accumulated status lines for IM display.
const maxStatusLines = 8

func appendStatusLine(lines []string, line string) []string {
	// Collapse consecutive thinking lines (replace last thinking with new one)
	if strings.HasPrefix(line, "💭") && len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "💭") {
		lines[len(lines)-1] = line
	} else {
		lines = append(lines, line)
	}
	if len(lines) > maxStatusLines {
		lines = lines[len(lines)-maxStatusLines:]
	}
	return lines
}
