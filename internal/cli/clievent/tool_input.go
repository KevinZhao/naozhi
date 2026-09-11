// tool_input.go — the human-readable one-line summary of a tool call's JSON
// input (#2649 G1-f).
//
// Moved out of internal/cli/process_event_format.go with the two helpers it owns:
// the per-tool input shapes and shortPath (whose only production callers are in
// FormatToolInput). internal/subagent needs this to render a subagent transcript,
// and it was the one thing keeping that package tied to the process manager.
//
// The rest of process_event_format.go stayed: EventEntriesFromEventAt,
// parseAgentInput and formatToolDetail are still used from cli's read loop.

package clievent

import (
	"encoding/json"
	"strings"

	"github.com/naozhi/naozhi/internal/textutil"
)

func shortPath(p string) string {
	const homePrefix = "/home/"
	if i := strings.Index(p, homePrefix); i >= 0 {
		rest := p[i+len(homePrefix):]
		if j := strings.Index(rest, "/"); j >= 0 {
			return "~" + rest[j:]
		}
	}
	if len(p) > 50 {
		// Snap to a rune boundary so CJK paths aren't sliced into invalid UTF-8.
		return "..." + p[textutil.TailAtRuneBoundary(p, len(p)-47):]
	}
	return p
}

// Per-tool input shapes for FormatToolInput, named at package level: the
// encoding/json reflection cache keys on the type, and an anonymous struct
// literal inside the function defeats reuse (reflect lookup + alloc per event).
type (
	toolInputFilePath struct {
		FilePath string `json:"file_path"`
	}
	toolInputPattern struct {
		Pattern string `json:"pattern"`
	}
	toolInputGrep struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	toolInputBash struct {
		Description string `json:"description"`
		Command     string `json:"command"`
	}
	toolInputAgent struct {
		Description string `json:"description"`
	}
	toolInputFallback struct {
		Description string `json:"description"`
		FilePath    string `json:"file_path"`
		Path        string `json:"path"`
		Command     string `json:"command"`
		Pattern     string `json:"pattern"`
		Prompt      string `json:"prompt"`
	}
)

// FormatToolInput extracts a human-readable summary from a tool's JSON input.
// Uses per-tool struct parsing to avoid map allocation on the hot path.
func FormatToolInput(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return toolName
	}

	switch toolName {
	case "Read", "Write", "Edit":
		var s toolInputFilePath
		if json.Unmarshal(input, &s) == nil && s.FilePath != "" {
			return toolName + " " + shortPath(s.FilePath)
		}
	case "Glob":
		var s toolInputPattern
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			// Cap so an adversarial LLM response cannot inflate ring.EventLog entries.
			return toolName + " " + textutil.TruncateRunes(s.Pattern, 300)
		}
	case "Grep":
		var s toolInputGrep
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			// Cap pattern (see Glob).
			result := toolName + " " + textutil.TruncateRunes(s.Pattern, 300)
			if s.Path != "" {
				result += " in " + shortPath(s.Path)
			}
			return result
		}
	case "Bash":
		var s toolInputBash
		if json.Unmarshal(input, &s) == nil {
			if s.Description != "" {
				return toolName + " " + s.Description
			}
			if s.Command != "" {
				return toolName + " " + textutil.TruncateRunes(s.Command, 80)
			}
		}
	case "Agent":
		var s toolInputAgent
		if json.Unmarshal(input, &s) == nil && s.Description != "" {
			return toolName + " " + textutil.TruncateRunes(s.Description, 60)
		}
	default:
		// Unknown tools: a concrete struct (json ignores unknown fields) beats a
		// map decode and still works for MCP tools with new schemas.
		var inp toolInputFallback
		if json.Unmarshal(input, &inp) == nil {
			switch {
			case inp.Description != "":
				return toolName + " " + textutil.TruncateRunes(inp.Description, 80)
			case inp.FilePath != "":
				return toolName + " " + textutil.TruncateRunes(inp.FilePath, 80)
			case inp.Path != "":
				return toolName + " " + textutil.TruncateRunes(inp.Path, 80)
			case inp.Command != "":
				return toolName + " " + textutil.TruncateRunes(inp.Command, 80)
			case inp.Pattern != "":
				return toolName + " " + textutil.TruncateRunes(inp.Pattern, 80)
			case inp.Prompt != "":
				return toolName + " " + textutil.TruncateRunes(inp.Prompt, 80)
			}
		}
	}

	// Pass the []byte directly so multi-KB MCP inputs don't pay a string copy.
	return toolName + ": " + textutil.TruncateRunesBytes(input, 300)
}
