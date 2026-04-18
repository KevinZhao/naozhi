package dispatch

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

// Per-tool input structs — zero-alloc alternative to generic map decoding.
type readInput struct {
	FilePath string `json:"file_path"`
}
type editInput struct {
	FilePath string `json:"file_path"`
}
type writeInput struct {
	FilePath string `json:"file_path"`
}
type bashInput struct {
	Command string `json:"command"`
}
type grepInput struct {
	Pattern string `json:"pattern"`
}
type globInput struct {
	Pattern string `json:"pattern"`
}
type agentInput struct {
	Description string `json:"description"`
}

func formatToolUse(name string, input json.RawMessage) string {
	switch name {
	case "Read":
		var s readInput
		if json.Unmarshal(input, &s) == nil && s.FilePath != "" {
			return "📖 " + shortenPath(s.FilePath)
		}
	case "Edit":
		var s editInput
		if json.Unmarshal(input, &s) == nil && s.FilePath != "" {
			return "✏️ " + shortenPath(s.FilePath)
		}
	case "Write":
		var s writeInput
		if json.Unmarshal(input, &s) == nil && s.FilePath != "" {
			return "📝 " + shortenPath(s.FilePath)
		}
	case "Bash":
		var s bashInput
		if json.Unmarshal(input, &s) == nil && s.Command != "" {
			return "⚡ " + cli.TruncateRunes(s.Command, 50)
		}
	case "Grep":
		var s grepInput
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			return "🔍 grep " + cli.TruncateRunes(s.Pattern, 40)
		}
	case "Glob":
		var s globInput
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			return "🔍 " + cli.TruncateRunes(s.Pattern, 40)
		}
	case "Agent":
		var s agentInput
		if json.Unmarshal(input, &s) == nil && s.Description != "" {
			return "🤖 " + s.Description
		}
	}
	// Fallback: ACP tool_call titles or unknown tools
	return "🔧 " + name
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
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		first := strings.TrimSpace(s[:i])
		if first != "" {
			return first
		}
		rest := strings.TrimSpace(s[i+1:])
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			return strings.TrimSpace(rest[:j])
		}
		return rest
	}
	return s
}

// statusAccumulator tracks accumulated status lines for IM display.
const maxStatusLines = 8

// appendStatusLine adds a status line, collapsing consecutive thinking lines.
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
