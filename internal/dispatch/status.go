package dispatch

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/textutil"
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
			// First line of thinking, truncated; strip escape bytes (#836) since
			// thinking text may echo terminal output.
			first := textutil.FirstLine(block.Text)
			return "💭 " + textutil.TruncateRunes(stripANSI(first), 50)
		case "tool_use":
			return stripANSI(formatToolUse(block.Name, block.Input))
		}
	}
	return ""
}

// extractTodoMessage returns the rendered checklist text for a TodoWrite
// tool_use block, or ("", false) if the event carries no TodoWrite update.
// Only the first TodoWrite block in the event is honoured — Claude never
// emits multiple TodoWrite calls in a single assistant message.
func extractTodoMessage(ev cli.Event) (string, bool) {
	if ev.Message == nil {
		return "", false
	}
	for _, block := range ev.Message.Content {
		if block.Type != "tool_use" || block.Name != "TodoWrite" {
			continue
		}
		todos, ok := cli.ParseTodos(block.Input)
		if !ok {
			return "", false
		}
		return cli.TodosMarkdown(todos), true
	}
	return "", false
}

// Per-tool input structs — zero-alloc alternative to generic map decoding.
// Read/Edit/Write share the file_path shape.
type filePathInput struct {
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

// TodoWrite is intentionally NOT handled here: dispatch.go onEvent sends the
// checklist as a standalone Reply so it gets its own chat bubble; the banner
// falls through to the generic "🔧 TodoWrite" marker.

func formatToolUse(name string, input json.RawMessage) string {
	// Read/Edit/Write share filePathInput: decode once, format per arm.
	switch name {
	case "Read", "Edit", "Write":
		var s filePathInput
		if json.Unmarshal(input, &s) != nil || s.FilePath == "" {
			break
		}
		switch name {
		case "Read":
			return "📖 " + shortenPath(s.FilePath)
		case "Edit":
			return "✏️ " + shortenPath(s.FilePath)
		case "Write":
			return "📝 " + shortenPath(s.FilePath)
		}
	case "Bash":
		var s bashInput
		if json.Unmarshal(input, &s) == nil && s.Command != "" {
			return "⚡ " + textutil.TruncateRunes(s.Command, 50)
		}
	case "Grep":
		var s grepInput
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			return "🔍 grep " + textutil.TruncateRunes(s.Pattern, 40)
		}
	case "Glob":
		var s globInput
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			return "🔍 " + textutil.TruncateRunes(s.Pattern, 40)
		}
	case "Agent":
		var s agentInput
		if json.Unmarshal(input, &s) == nil && s.Description != "" {
			// Agent.Description can be multi-line / long; truncate like the
			// other arms.
			return "🤖 " + textutil.TruncateRunes(s.Description, 50)
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

// maxStatusLines caps the status lines retained in the IM thinking banner.
const maxStatusLines = 8

// appendStatusLine adds a status line, collapsing consecutive thinking lines.
func appendStatusLine(lines []string, line string) []string {
	if strings.HasPrefix(line, "💭") && len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "💭") {
		lines[len(lines)-1] = line
	} else {
		lines = append(lines, line)
	}
	if len(lines) > maxStatusLines {
		// copy-to-front instead of reslicing so the backing array's head isn't
		// abandoned (would leak capacity each drop on a long turn).
		copy(lines, lines[len(lines)-maxStatusLines:])
		lines = lines[:maxStatusLines]
	}
	return lines
}
