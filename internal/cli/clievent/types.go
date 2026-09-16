// Package clievent holds the leaf event-record types shared between the CLI
// wrapper, persistence, discovery, and dashboard layers, breaking the diamond
// import session → discovery → cli ← session (#626). New record types land
// here only when they are pure data shapes consumed by ≥2 packages above cli.
package clievent

import "strings"

// EventEntry is the event record shared by the CLI wrapper, EventLog ring
// buffer, JSONL persistence and the dashboard renderer; persisted JSON in
// `<dataDir>/sessions/*.jsonl` round-trips through this struct.
type EventEntry struct {
	// UUID is a 32-char lowercase hex identity assigned by EventLog.stampUUID
	// and persisted; MergedSource's exact-match dedup key between the local
	// JSONL tier and Claude CLI JSONL fallback. "" = legacy entry (MergedSource
	// derives a stable UUID from Time + Summary).
	UUID       string   `json:"uuid,omitempty"`
	Time       int64    `json:"time"`                 // unix ms
	Type       string   `json:"type"`                 // init, thinking, tool_use, text, result, system, agent, todo, task_start, task_progress (also maps task_updated), task_done
	Summary    string   `json:"summary,omitempty"`    // brief description
	Cost       float64  `json:"cost,omitempty"`       // cumulative cost (result events only)
	Detail     string   `json:"detail,omitempty"`     // fuller content for terminal view
	Tool       string   `json:"tool,omitempty"`       // tool name for tool_use events
	Subagent   string   `json:"subagent,omitempty"`   // subagent_type or name (empty for team-only agents)
	TeamName   string   `json:"team_name,omitempty"`  // team grouping key for agent team members
	Background bool     `json:"background,omitempty"` // true for run_in_background team agents
	Images     []string `json:"images,omitempty"`     // thumbnail data URIs for user image uploads
	// ImagePaths is the workspace-relative on-disk copy of each inline image,
	// index-aligned with Images (empty slot → thumbnail fallback); used by the
	// lightbox via /api/sessions/attachment. ALWAYS sanitized before use: the
	// HTTP handler rejects absolute/escaping paths; persisted history is pass-through.
	ImagePaths []string `json:"image_paths,omitempty"`
	TaskID     string   `json:"task_id,omitempty"`     // agent task correlation ID
	ToolUseID  string   `json:"tool_use_id,omitempty"` // links Agent tool_use → task_started
	LastTool   string   `json:"last_tool,omitempty"`   // most recent tool in agent task
	ToolUses   int      `json:"tool_uses,omitempty"`   // tool call count in agent task
	Tokens     int      `json:"tokens,omitempty"`      // total tokens consumed by agent task
	DurationMS int64    `json:"duration_ms,omitempty"` // elapsed ms for agent task
	Status     string   `json:"status,omitempty"`      // agent task status (completed, error, etc.)
	// Agent team internal-view linkage, persisted on "agent" and "task_start"
	// entries so SubagentLinker.SeedFromHistory can rebuild the task_id →
	// transcript mapping after reconnect/respawn without re-scanning
	// ~/.claude/projects/. Async backfilled via EventLog.SetAgentInternalID.
	TaskType        string `json:"task_type,omitempty"`         // "in_process_teammate" | "local_bash" | ""
	InternalAgentID string `json:"internal_agent_id,omitempty"` // "agent-<hex17>" filename stem under <projectDir>/<sessionID>/subagents/
	JSONLPath       string `json:"jsonl_path,omitempty"`        // absolute path to agent transcript jsonl
	FirstPromptID   string `json:"first_prompt_id,omitempty"`   // jsonl first-line promptId; guards against same-name re-spawn

	// AskQuestion carries the AskUserQuestion card payload on Type=="ask_question"
	// entries; a typed field so dashboard and replay callers never re-parse Detail.
	AskQuestion *AskQuestion `json:"ask_question,omitempty"`

	// ToolCall is the per-event payload for ACP tool_call / tool_call_update
	// rich progress rows; the dashboard threads them by ID. Stream-json
	// (Claude) leaves nil and uses Type=="tool_use" with Tool + Detail.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
}

// ToolCall is the per-event payload for ACP tool_call / tool_call_update
// session/update notifications. The same struct serves the initial
// invocation (Status "" or "pending") and updates; successive events with the
// same ID overwrite the prior progress row. Output stays the raw JSON kiro
// emits so "view raw" preserves formatting.
type ToolCall struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`        // "execute" / "read" / "write" / "search" / vendor-specific
	Status     string `json:"status,omitempty"`      // "" (initial) / "in_progress" / "completed" / "failed"
	InputJSON  string `json:"input_json,omitempty"`  // raw JSON of rawInput
	OutputJSON string `json:"output_json,omitempty"` // raw JSON of rawOutput
}

// AskQuestion mirrors the shape of AskUserQuestion.input (see
// test/e2e/askuser/aq1_aq2_trigger_and_schema.py). ToolUseID correlates
// dashboard + IM renderings of the same question.
type AskQuestion struct {
	ToolUseID string            `json:"tool_use_id"`
	Items     []AskQuestionItem `json:"items"`
}

// AskQuestionItem is one question in a possibly multi-question card.
// MultiSelect=true signals checkbox semantics; the dashboard currently
// degrades to single-select.
type AskQuestionItem struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
	Options     []AskQuestionOpt `json:"options"`
}

// AskQuestionOpt is one selectable choice. Label is echoed back by the answer
// composer ("Header: Label."); Description is tooltip-only, never echoed.
type AskQuestionOpt struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// SchemaVersion is the semantic version of the EventEntry shape shared by
// the three surfaces that carry it: the persisted event log
// (eventlog/schema.WireVersion pairs with it), the node RPC (the register
// handshake advertises "evententry.v<N>" so a peer can gate a future
// semantic change), and the dashboard WS wire.
//
// Bump policy (mirrors eventlog/schema.WireVersion):
//   - Additive fields with omitempty → NO bump; readers ignore unknowns.
//   - Renaming/removing a field, changing a field's meaning, or changing
//     its JSON shape → bump, together with the paired constants (the
//     linkage tests in eventlog/schema, internal/node and internal/upstream
//     go red until every pairing is updated deliberately).
const SchemaVersion = 1

// SchemaCap is the capability tag both sides of the reverse-node link advertise
// for this EventEntry schema — the node on its register frame, the hub on the
// registered ack. It must never appear in any backend.Profile.RequiredNodeCaps:
// it describes what the peer speaks, not what a backend needs, so it is gated by
// SchemaCapMismatch rather than by the RequiredNodeCaps loop.
const SchemaCap = "evententry.v1"

// schemaCapPrefix is the family SchemaCap belongs to. A peer advertising a
// different member of the family speaks a different EventEntry shape.
const schemaCapPrefix = "evententry.v"

// SchemaCapMismatch reports the peer's EventEntry schema tag when it names a
// version this binary cannot read, and "" when the link is safe to use.
//
// Two cases are deliberately NOT a mismatch:
//
//   - no evententry tag at all — a peer predating capability negotiation. It is
//     speaking v1 by construction (the tag was introduced alongside v1), and
//     refusing it would break the very upgrade path the tag exists to smooth.
//   - our own tag, however many other unknown caps accompany it. Unknown caps
//     stay a WARN: they describe features, and a feature this binary does not
//     know is one it will not ask for.
//
// A mismatch is different in kind: EventEntry is the payload of the `event` and
// `events` frames themselves, so a peer on another version does not lack a
// feature — every event it sends decodes into the wrong shape, silently. That is
// why this is the one cap whose disagreement ends the connection instead of
// logging.
// A peer advertising several members of the family (a transitional build that
// reads both) is compatible as long as ours is among them, so the whole slice is
// scanned before any of it is rejected.
func SchemaCapMismatch(peerCaps []string) string {
	foreign := ""
	for _, c := range peerCaps {
		if !strings.HasPrefix(c, schemaCapPrefix) {
			continue
		}
		if c == SchemaCap {
			return ""
		}
		if foreign == "" {
			foreign = c
		}
	}
	return foreign
}
