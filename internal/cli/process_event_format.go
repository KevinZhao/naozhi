package cli

// process_event_format.go — Event → EventEntry conversion and tool
// input formatting.
//
// Moved from process.go (Phase 5 of docs/rfc/process-split.md).
// Zero semantic change; pure file move.
//
// Almost all functions here are pure: the only non-pure one is
// logEventAt, which pairs conversion with an EventLog.AppendBatch
// side effect and a result-cost atomic update.

import (
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// EventEntryFromEvent converts an Event to a single EventEntry.
// Deprecated for multi-block assistant events — use EventEntriesFromEvent.
// Kept for callers that only need the first entry (or non-assistant events).
func EventEntryFromEvent(ev Event) (EventEntry, bool) {
	entries := EventEntriesFromEvent(ev)
	if len(entries) == 0 {
		return EventEntry{}, false
	}
	return entries[0], true
}

// EventEntriesFromEvent converts an Event to zero or more EventEntry values.
// Assistant messages can contain multiple content blocks (thinking + tool_use + text);
// each block that maps to a known type produces its own entry so downstream consumers
// (EventLog, dashboard) don't silently drop blocks after the first.
func EventEntriesFromEvent(ev Event) []EventEntry {
	return EventEntriesFromEventAt(ev, time.Now().UnixMilli())
}

// EventEntriesFromEventAt is the caller-supplied-now variant used by readLoop
// to share a single time.Now() call between ev.recvAt assignment and entry
// timestamping. Public callers still use EventEntriesFromEvent. R67-PERF-9.
func EventEntriesFromEventAt(ev Event, nowMS int64) []EventEntry {
	// Replay events are a passthrough-internal CLI ack for messages naozhi
	// already showed to the user via the optimistic bubble. Writing them to
	// EventLog causes double-display on the dashboard. readLoop already
	// short-circuits replay events before logEventAt, but belt-and-suspenders:
	// if any future caller passes a replay directly, still skip.
	if ev.Type == "user" && ev.IsReplay {
		return nil
	}
	now := nowMS
	base := EventEntry{Time: now}

	switch ev.Type {
	case "system":
		entry := base
		entry.Type = "system"
		entry.Summary = ev.SubType
		if ev.SubType == "init" {
			return nil
		}
		switch ev.SubType {
		case "task_started":
			entry.Type = "task_start"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			entry.TaskType = ev.TaskType
			if ev.Description != "" {
				entry.Summary = textutil.TruncateRunes(ev.Description, 120)
			}
		case "task_progress", "task_updated":
			entry.Type = "task_progress"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			if ev.Description != "" {
				entry.Summary = textutil.TruncateRunes(ev.Description, 120)
			}
			entry.LastTool = ev.LastToolName
			if ev.Usage != nil {
				entry.ToolUses = ev.Usage.ToolUses
				entry.Tokens = ev.Usage.TotalTokens
				entry.DurationMS = ev.Usage.DurationMS
			}
		case "task_notification":
			entry.Type = "task_done"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			if ev.Description != "" {
				entry.Summary = textutil.TruncateRunes(ev.Description, 120)
			}
			entry.Status = ev.Status
			if ev.Usage != nil {
				entry.ToolUses = ev.Usage.ToolUses
				entry.Tokens = ev.Usage.TotalTokens
				entry.DurationMS = ev.Usage.DurationMS
			}
		case "stop_hook_summary", "turn_duration", "hook_started", "hook_response":
			return nil
		}
		return []EventEntry{entry}
	case "assistant":
		if ev.Message == nil {
			return nil
		}
		// Pre-size to the content block count: single-block events pay 1
		// alloc (same as the old nil+append path), and multi-block events
		// (thinking+tool_use+text) avoid 2-3 append-driven growth reallocs.
		out := make([]EventEntry, 0, len(ev.Message.Content))
		for _, block := range ev.Message.Content {
			entry := base
			switch block.Type {
			case "thinking":
				entry.Type = "thinking"
				entry.Summary = textutil.TruncateRunes(block.Text, 120)
				entry.Detail = textutil.TruncateRunes(block.Text, 2000)
			case "tool_use":
				entry.Type = "tool_use"
				entry.Summary = block.Name
				entry.Tool = block.Name
				switch block.Name {
				case "Agent":
					inp := parseAgentInput(block.Input)
					entry.Type = "agent"
					entry.Subagent = inp.SubagentType
					if entry.Subagent == "" {
						entry.Subagent = inp.Name
					}
					entry.TeamName = inp.TeamName
					entry.Summary = textutil.TruncateRunes(inp.Description, 120)
					entry.Background = inp.RunInBackground
					entry.ToolUseID = block.ID
					// R217-PERF-9: derive Detail from already-parsed
					// agentInput to skip a second json.Unmarshal of
					// block.Input via formatToolDetail →
					// FormatToolInput("Agent", ...). The output mirrors the
					// FormatToolInput Agent branch:
					//   "Agent " + truncate(description, 60), or "Agent"
					// when description is empty / input is malformed.
					if inp.Description != "" {
						entry.Detail = "Agent " + textutil.TruncateRunes(inp.Description, 60)
					} else {
						entry.Detail = "Agent"
					}
				case "TodoWrite":
					entry.Detail = formatToolDetail(block)
					if todos, ok := ParseTodos(block.Input); ok {
						entry.Type = "todo"
						entry.Tool = "TodoWrite"
						entry.Summary = TodosSummary(todos)
						// Dashboard renderTodoList expects a JSON array of
						// TodoItem, not the full `{"todos":[...]}` envelope
						// that block.Input carries. Marshal the decoded slice
						// so the frontend sees `[{...}, {...}]` and renders
						// the checklist; otherwise JSON.parse yields an
						// object, Array.isArray returns false, and the UI
						// silently falls back to the one-line summary.
						entry.Detail = TodosDetailJSON(todos)
					}
				default:
					entry.Detail = formatToolDetail(block)
				}
			case "text":
				entry.Type = "text"
				entry.Summary = textutil.TruncateRunes(block.Text, 120)
				entry.Detail = textutil.TruncateRunes(block.Text, 16000)
			default:
				continue
			}
			out = append(out, entry)
		}
		// Surface the AskUserQuestion card as a dedicated EventEntry alongside
		// the tool_use bubble. Placing it AFTER the tool_use entry keeps the
		// chronological transcript order (tool_use → interactive card) and
		// preserves the Agent → task_started tool_use_id linkage above.
		if ev.AskQuestion != nil {
			entry := base
			entry.Type = "ask_question"
			entry.Tool = "AskUserQuestion"
			entry.ToolUseID = ev.AskQuestion.ToolUseID
			// Summary is a one-line digest used for sidebar preview;
			// AskQuestion field carries the full card payload.
			if len(ev.AskQuestion.Items) > 0 {
				entry.Summary = textutil.TruncateRunes(ev.AskQuestion.Items[0].Question, 120)
			} else {
				entry.Summary = "AskUserQuestion"
			}
			entry.AskQuestion = ev.AskQuestion
			out = append(out, entry)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case "result":
		entry := base
		entry.Type = "result"
		entry.Summary = textutil.TruncateRunes(ev.Result, 200)
		entry.Detail = textutil.TruncateRunes(ev.Result, 16000)
		entry.Cost = ev.CostUSD
		return []EventEntry{entry}
	}
	return nil
}

// logEventAt converts an Event to one or more EventEntry values and appends them to the event log.
// readLoop passes the same time.Now() value that stamps ev.recvAt so timestamps match. R67-PERF-9.
func (p *Process) logEventAt(ev Event, nowMS int64) {
	entries := EventEntriesFromEventAt(ev, nowMS)
	if len(entries) == 0 {
		return
	}
	// Update process-level cost tracking for result events.
	if ev.Type == "result" {
		p.totalCost.Store(math.Float64bits(ev.CostUSD))
	}
	// AppendBatch holds l.mu and notifies subscribers ONCE rather than
	// once per entry. Multi-block assistant events (thinking + tool_use +
	// text) would otherwise acquire both locks N times and wake
	// eventPushLoop spuriously for each block.
	p.eventLog.AppendBatch(entries)
}

// agentInput holds the parsed fields from an Agent tool call input.
type agentInput struct {
	SubagentType    string `json:"subagent_type"`
	Name            string `json:"name"`
	TeamName        string `json:"team_name"`
	Description     string `json:"description"`
	RunInBackground bool   `json:"run_in_background"`
}

func parseAgentInput(input json.RawMessage) agentInput {
	if len(input) == 0 {
		return agentInput{}
	}
	var inp agentInput
	if err := json.Unmarshal(input, &inp); err != nil {
		// R188-PANIC-H1: upgrade from Debug to Warn. Silent zero-value return
		// produces blank agent cards in the dashboard; at Warn operators can
		// trace which CLI emitted a malformed Agent.input and whether the
		// schema drifted (e.g. CLI emitting "input": "string" instead of
		// {"subagent_type": ...}).
		slog.Warn("parseAgentInput: unmarshal failed",
			"err", err, "input_len", len(input))
	}
	return inp
}

// label returns the preferred human-readable identifier for an Agent tool call.
// Used by tests that lock the Agent event formatting contract.
func (a agentInput) label() string {
	if a.SubagentType != "" {
		return a.SubagentType
	}
	if a.Name != "" {
		return a.Name
	}
	return a.TeamName
}

func formatToolDetail(block ContentBlock) string {
	if len(block.Input) == 0 {
		return block.Name
	}
	return FormatToolInput(block.Name, block.Input)
}

func shortPath(p string) string {
	const homePrefix = "/home/"
	if i := strings.Index(p, homePrefix); i >= 0 {
		rest := p[i+len(homePrefix):]
		if j := strings.Index(rest, "/"); j >= 0 {
			return "~" + rest[j:]
		}
	}
	if len(p) > 50 {
		return "..." + p[len(p)-47:]
	}
	return p
}

// FormatToolInput extracts a human-readable summary from a tool's JSON input.
// Uses per-tool struct parsing to avoid map allocation on the hot path.
func FormatToolInput(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return toolName
	}

	switch toolName {
	case "Read", "Write", "Edit":
		var s struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(input, &s) == nil && s.FilePath != "" {
			return toolName + " " + shortPath(s.FilePath)
		}
	case "Glob":
		var s struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			// R187-PERF-L1: cap pattern to prevent an adversarial LLM response
			// from inflating EventLog entries (300 runes matches the default
			// tail below).
			return toolName + " " + textutil.TruncateRunes(s.Pattern, 300)
		}
	case "Grep":
		var s struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			// R187-PERF-L1: cap pattern (see Glob note).
			result := toolName + " " + textutil.TruncateRunes(s.Pattern, 300)
			if s.Path != "" {
				result += " in " + shortPath(s.Path)
			}
			return result
		}
	case "Bash":
		var s struct {
			Description string `json:"description"`
			Command     string `json:"command"`
		}
		if json.Unmarshal(input, &s) == nil {
			if s.Description != "" {
				return toolName + " " + s.Description
			}
			if s.Command != "" {
				return toolName + " " + textutil.TruncateRunes(s.Command, 80)
			}
		}
	case "Agent":
		var s struct {
			Description string `json:"description"`
		}
		if json.Unmarshal(input, &s) == nil && s.Description != "" {
			return toolName + " " + textutil.TruncateRunes(s.Description, 60)
		}
	default:
		// R188-PERF-P2-C: replaced map[string]json.RawMessage decode with
		// concrete struct — json.Decoder ignores unknown fields by default so
		// MCP tools that add new schemas still work, and we skip the reflect
		// + map alloc cost on the unknown-tool fallback path.
		// Fallback: try common keys with a struct (rare path for unknown tools)
		var inp struct {
			Description string `json:"description"`
			FilePath    string `json:"file_path"`
			Path        string `json:"path"`
			Command     string `json:"command"`
			Pattern     string `json:"pattern"`
			Prompt      string `json:"prompt"`
		}
		if json.Unmarshal(input, &inp) == nil {
			// Avoid a []string{...} slice literal on the unknown-tool
			// fallback path — a chain of short-circuit checks matches the
			// previous semantics without the per-call slice alloc.
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

	return toolName + ": " + textutil.TruncateRunes(string(input), 300)
}
