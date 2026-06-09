package cli

// process_event_format.go — Event → EventEntry conversion and tool
// input formatting.
//
// Owns pure conversion helpers + logEventAt (the only non-pure entry,
// pairing conversion with EventLog.AppendBatch and the result-cost
// atomic update).
//
// R227-ARCH-19: dropped the "Phase 5 of process-split / zero semantic
// change" preamble — process-split landed in 2026-05 and the file is
// no longer in flux; the historical context is preserved in git log.

import (
	"encoding/json"
	"log/slog"
	"math"
	"strings"

	"github.com/naozhi/naozhi/internal/textutil"
)

// EventEntriesFromEventAt converts an Event to zero or more EventEntry values
// using a caller-supplied wall-clock to share a single time.Now() call between
// ev.recvAt assignment and entry timestamping. Assistant messages can contain
// multiple content blocks (thinking + tool_use + text); each block that maps
// to a known type produces its own entry so downstream consumers (EventLog,
// dashboard) don't silently drop blocks after the first. R67-PERF-9.
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
		// ACP tool_call_update events (Sprint 5c / RFC §8.3 D17) carry no
		// Message content — they're pure status/output progress updates
		// for an existing tool_use bubble. Synthesise a thin entry so the
		// dashboard can thread it onto the prior tool_use by ToolUseID.
		if ev.SubType == "tool_result" && ev.ToolCall != nil {
			entry := base
			entry.Type = "tool_use"
			entry.ToolUseID = ev.ToolUseID
			if ev.ToolCall.Title != "" {
				entry.Tool = ev.ToolCall.Title
				entry.Summary = ev.ToolCall.Title
			}
			// R230-PERF-2: share the ToolCall pointer rather than copying
			// the struct. ev.ToolCall is freshly allocated by ACPProtocol
			// per event and never mutated after EventEntriesFromEventAt
			// returns; downstream consumers (eventlog Append, dashboard
			// Marshal) only read its fields. Append takes EventEntry by
			// value so the pointer is shared but the entry struct stays
			// owned by the ring buffer.
			entry.ToolCall = ev.ToolCall
			return []EventEntry{entry}
		}
		if ev.Message == nil {
			return nil
		}
		// Skip the make() entirely when Content is empty — avoids the
		// zero-cap slice-header alloc on rare empty thinking blocks.
		if len(ev.Message.Content) == 0 {
			return nil
		}
		// Pre-size to the content block count: single-block events pay 1
		// alloc (same as the old nil+append path), and multi-block events
		// (thinking+tool_use+text) avoid 2-3 append-driven growth reallocs.
		out := make([]EventEntry, 0, len(ev.Message.Content))
		for _, block := range ev.Message.Content {
			// R229-PERF-3: skip unknown block types BEFORE paying the
			// `entry := base` struct copy (~240 B per iteration). The
			// switch below already had `default: continue` at the tail —
			// hoisting the same predicate avoids the wasted struct copy
			// on event streams that mix thinking blocks with future
			// unknown content kinds.
			switch block.Type {
			case "thinking", "tool_use", "text":
			default:
				continue
			}
			entry := base
			switch block.Type {
			case "thinking":
				entry.Type = "thinking"
				// R20260602190132-PERF-11: one UTF-8 scan derives both the
				// 120-rune Summary and the eventDetailMaxRunes Detail rather
				// than scanning block.Text twice from the head.
				entry.Summary, entry.Detail = textutil.TruncateRunesPair(block.Text, 120, eventDetailMaxRunes)
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
					if todos, rawTodos, ok := ParseTodosWithRaw(block.Input); ok {
						entry.Type = "todo"
						entry.Tool = "TodoWrite"
						entry.Summary = TodosSummary(todos)
						// Dashboard renderTodoList expects a JSON array of
						// TodoItem, not the full `{"todos":[...]}` envelope
						// that block.Input carries. R226-PERF-8: reuse the
						// already-parsed RawMessage from block.Input instead
						// of re-marshalling the typed slice — the frontend
						// sees `[{...}, {...}]` either way, but we save the
						// per-event Marshal+copy.
						entry.Detail = string(rawTodos)
					}
				default:
					entry.Detail = formatToolDetail(block)
				}
				// Multi-Backend RFC §8.3 D17: ACP tool_call events bring a
				// rich progress payload (kind / status / rawOutput) that
				// the dashboard renders as a folded "▶ <title>: status →
				// stdout" row. Forward the per-event ToolCall so the
				// frontend doesn't have to re-derive it. Stream-json
				// (Claude) leaves ev.ToolCall nil and uses the legacy
				// Tool / Detail fields.
				if ev.ToolCall != nil {
					// R230-PERF-2: share the ToolCall pointer; see the
					// matching tool_result branch above for the safety
					// argument (read-only downstream, EventEntry stored
					// by value in the ring buffer).
					entry.ToolCall = ev.ToolCall
				}
			case "text":
				entry.Type = "text"
				// R20260602190132-PERF-11: single-scan dual truncation; see
				// the thinking branch above.
				entry.Summary, entry.Detail = textutil.TruncateRunesPair(block.Text, 120, 16000)
			}
			// No default branch: the outer guard switch above already
			// `continue`s on unknown block.Type, so anything reaching this
			// switch is one of the three handled kinds.
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
		// Result is purely turn-boundary metadata: cost, stopReason, sessionId.
		// The visible assistant text was already logged by the assistant frames
		// preceding this event — claude streams text content blocks turn-mid,
		// and ACPProtocol.ReadEvent synthesises an assistant frame at stopReason
		// (see protocol_acp.go) so both backends share the same invariant.
		//
		// Frontend dashboard.js INTERNAL_EVENT_TYPES filters "result" out so
		// it never renders a bubble; ev.Result is preserved on the wire for
		// process_send.Send() (passthrough SendResult.Text) but is not copied
		// into Summary/Detail — earlier code that derived a parallel "text"
		// entry from ev.Result produced a duplicate bubble whenever an
		// assistant text frame had already been logged in the same turn.
		entry := base
		entry.Type = "result"
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
		// Snap the tail start to a rune boundary so a multi-byte (e.g. CJK)
		// path segment isn't sliced mid-codepoint into invalid UTF-8. R20260609-LB-2.
		return "..." + p[textutil.TailAtRuneBoundary(p, len(p)-47):]
	}
	return p
}

// Per-tool input shapes for FormatToolInput. Promoted from anonymous struct
// literals inside the function body to package-level named types: the encoding/json
// reflection cache keys on (Type, []byte) and a fresh anonymous-type literal
// inside the function defeats reuse, costing a fresh reflect lookup + a small
// alloc for the type's name on every event. Naming the types preserves the
// "tool's payload shape lives next to the dispatch switch" readability while
// stabilising the cache key. R229-PERF-2.
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
			// R187-PERF-L1: cap pattern to prevent an adversarial LLM response
			// from inflating EventLog entries (300 runes matches the default
			// tail below).
			return toolName + " " + textutil.TruncateRunes(s.Pattern, 300)
		}
	case "Grep":
		var s toolInputGrep
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			// R187-PERF-L1: cap pattern (see Glob note).
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
		// R188-PERF-P2-C: replaced map[string]json.RawMessage decode with
		// concrete struct — json.Decoder ignores unknown fields by default so
		// MCP tools that add new schemas still work, and we skip the reflect
		// + map alloc cost on the unknown-tool fallback path.
		// Fallback: try common keys with a struct (rare path for unknown tools)
		var inp toolInputFallback
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

	// R215-PERF-P2-6: pass the underlying []byte directly instead of
	// string(input) so MCP tools whose input is multi-KB don't pay a full
	// heap copy on every event when the truncation path is the common case.
	return toolName + ": " + textutil.TruncateRunesBytes(input, 300)
}
