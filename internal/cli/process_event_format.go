package cli

// process_event_format.go — Event → EventEntry conversion and tool input
// formatting; logEventAt is the only non-pure entry (AppendBatch + cost atomic).

import (
	"encoding/json"
	"log/slog"
	"math"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// EventEntriesFromEventAt converts an Event to zero or more EventEntry values,
// stamped with the caller-supplied wall-clock (shared with ev.recvAt). Each
// known content block of an assistant message (thinking / tool_use / text)
// yields its own entry so downstream consumers don't drop blocks after the first.
func EventEntriesFromEventAt(ev Event, nowMS int64) []clievent.EventEntry {
	// Replay events are the CLI's ack for a message already shown via the
	// optimistic bubble; logging them double-displays. readLoop skips them too.
	if ev.Type == "user" && ev.IsReplay {
		return nil
	}
	now := nowMS
	base := clievent.EventEntry{Time: now}

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
		case "stop_hook_summary", "turn_duration", "hook_started", "hook_response",
			// 1p auth (e.g. fable-5) streams these telemetry events many times per
			// turn; un-skipped they render as bare ⚙ rows. Drop at the source so they
			// never enter EventLog.
			"thinking_tokens", "background_tasks_changed":
			return nil
		}
		return []clievent.EventEntry{entry}
	case "assistant":
		// ACP tool_call_update events carry no Message content — pure progress for
		// an existing tool_use bubble. Synthesise a thin entry the dashboard threads
		// onto the prior tool_use by ToolUseID (Multi-Backend RFC §8.3 D17).
		if ev.SubType == "tool_result" && ev.ToolCall != nil {
			entry := base
			entry.Type = "tool_use"
			entry.ToolUseID = ev.ToolUseID
			if ev.ToolCall.Title != "" {
				entry.Tool = ev.ToolCall.Title
				entry.Summary = ev.ToolCall.Title
			}
			// Share the ToolCall pointer: ACPProtocol allocates it fresh per event and
			// downstream (eventlog Append, dashboard Marshal) only reads it.
			entry.ToolCall = ev.ToolCall
			return []clievent.EventEntry{entry}
		}
		if ev.Message == nil {
			return nil
		}
		// Skip the make() when Content is empty (rare empty thinking blocks).
		if len(ev.Message.Content) == 0 {
			return nil
		}
		// Pre-size so multi-block events avoid append-driven reallocs.
		out := make([]clievent.EventEntry, 0, len(ev.Message.Content))
		for _, block := range ev.Message.Content {
			// Skip unknown block types BEFORE the ~240 B `entry := base` copy.
			switch block.Type {
			case "thinking", "tool_use", "text":
			default:
				continue
			}
			entry := base
			switch block.Type {
			case "thinking":
				entry.Type = "thinking"
				// One UTF-8 scan derives both Summary and Detail.
				entry.Summary, entry.Detail = textutil.TruncateRunesPair(block.Text, 120, EventDetailMaxRunes)
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
					// Reuse the parsed agentInput instead of a second Unmarshal via
					// formatToolDetail; output mirrors FormatToolInput's Agent branch.
					if inp.Description != "" {
						entry.Detail = "Agent " + textutil.TruncateRunes(inp.Description, 60)
					} else {
						entry.Detail = "Agent"
					}
				case "TodoWrite":
					entry.Detail = formatToolDetail(block)
					if todos, rawTodos, ok := ParseTodosWithRaw(block.Input); ok {
						entry.Type = "todo"
						entry.Tool = "TodoWrite"
						entry.Summary = TodosSummary(todos)
						// Dashboard renderTodoList expects a JSON array of TodoItem, not the
						// `{"todos":[...]}` envelope; reuse the parsed RawMessage (no re-Marshal).
						entry.Detail = string(rawTodos)
					}
				default:
					entry.Detail = formatToolDetail(block)
				}
				// ACP tool_call events carry a progress payload (kind / status / rawOutput)
				// the dashboard renders as a folded row; forward it. Stream-json leaves
				// ev.ToolCall nil and uses the legacy Tool / Detail fields (RFC §8.3 D17).
				if ev.ToolCall != nil {
					// Shared pointer; see the tool_result branch above.
					entry.ToolCall = ev.ToolCall
				}
			case "text":
				entry.Type = "text"
				// Single-scan dual truncation; see the thinking branch.
				entry.Summary, entry.Detail = textutil.TruncateRunesPair(block.Text, 120, 16000)
			}
			// No default: the guard switch above already skips unknown block.Type.
			out = append(out, entry)
		}
		// AskUserQuestion card as its own entry, AFTER the tool_use so transcript
		// order and the Agent → task_started tool_use_id linkage are preserved.
		if ev.AskQuestion != nil {
			entry := base
			entry.Type = "ask_question"
			entry.Tool = "AskUserQuestion"
			entry.ToolUseID = ev.AskQuestion.ToolUseID
			// Summary is the sidebar digest; AskQuestion carries the full card.
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
		// Result is turn-boundary metadata only. The visible text was already logged
		// by preceding assistant frames (ACPProtocol.ReadEvent synthesises one at
		// stopReason, so both backends share the invariant); copying ev.Result into
		// Summary/Detail would duplicate the bubble. dashboard.js never renders "result".
		entry := base
		entry.Type = "result"
		entry.Cost = ev.CostUSD
		return []clievent.EventEntry{entry}
	}
	return nil
}

// logEventAt converts an Event to one or more EventEntry values and appends them to the event log.
// readLoop passes the same time.Now() value that stamps ev.recvAt so timestamps match.
func (p *Process) logEventAt(ev Event, nowMS int64) {
	entries := EventEntriesFromEventAt(ev, nowMS)
	if len(entries) == 0 {
		return
	}
	if ev.Type == "result" {
		p.totalCost.Store(math.Float64bits(ev.CostUSD))
	}
	// AppendBatch takes l.mu and notifies subscribers ONCE; per-entry Append
	// would lock N times and wake eventPushLoop spuriously per block.
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
		// Warn, not Debug: a silent zero-value return produces blank agent cards
		// and hides which CLI emitted a malformed Agent.input.
		slog.Warn("parseAgentInput: unmarshal failed",
			"err", err, "input_len", len(input))
	}
	return inp
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
			// Cap so an adversarial LLM response cannot inflate EventLog entries.
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
