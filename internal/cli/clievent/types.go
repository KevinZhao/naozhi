// Package clievent holds the leaf event-record types shared between the
// CLI wrapper, persistence, discovery, and dashboard layers.
//
// Splitting these out of `internal/cli` breaks the diamond import that
// would otherwise form: session → discovery → cli ← session. R217-ARCH-3
// (#626). The cli package re-exports each type via a Go type alias so
// existing call sites that say `cli.EventEntry` keep compiling without
// any rewrite — only packages that historically imported cli purely for
// these record types (today: `internal/discovery`) can now bind to the
// leaf and stop pulling in the rest of the cli surface.
//
// New record types should land here only when they are pure data shapes
// with no behaviour and are consumed by ≥2 packages above the cli
// boundary. Anything that needs cli-internal helpers stays in cli.
package clievent

// EventEntry is a simplified event record for the dashboard. The CLI
// wrapper, EventLog ring buffer, JSONL persistence layer, and the
// dashboard renderer all share this shape; persisted JSON in
// `<dataDir>/sessions/*.jsonl` is round-tripped through this struct.
//
// Field semantics are documented inline below; they were lifted as-is
// from internal/cli/eventlog.go (R217-ARCH-3 #626 — diamond-import
// extraction). cli.EventEntry remains as a type alias for backward
// compatibility with the ~365 existing call sites.
type EventEntry struct {
	// UUID is a 32-char lowercase hex identity for this event,
	// assigned at Append-time by EventLog.stampUUID. Stable across
	// process restarts because it rides along with the entry into
	// the on-disk event log (internal/eventlog/persist). MergedSource
	// uses UUID as the exact-match dedup key between the local
	// JSONL tier and Claude CLI JSONL fallback — see RFC §3.5.2.
	//
	// "" means "legacy entry (from a pre-UUID persisted record or
	// a Claude JSONL replay that hasn't been fingerprinted yet)".
	// MergedSource handles the empty case by deriving a stable UUID
	// from (Time + Summary) so two replays of the same Claude record
	// land on the same key.
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
	// ImagePaths is the workspace-relative path of the on-disk copy of each
	// inline image, index-aligned with Images. Populated opportunistically by
	// buildUserEntry when persistFileRefs persisted an image to the workspace
	// attachment directory. Consumed by the dashboard lightbox so clicking a
	// thumbnail can load the original via /api/sessions/attachment instead of
	// the downsampled data URI. An empty slot (e.g. persist failed, or a
	// legacy replayed event) falls back to the thumbnail. ALWAYS sanitized
	// before use: callers join it under the session workspace and must reject
	// any absolute or escaping path — validation lives in the HTTP handler,
	// not here, so persisted history is pass-through.
	ImagePaths []string `json:"image_paths,omitempty"`
	TaskID     string   `json:"task_id,omitempty"`     // agent task correlation ID
	ToolUseID  string   `json:"tool_use_id,omitempty"` // links Agent tool_use → task_started
	LastTool   string   `json:"last_tool,omitempty"`   // most recent tool in agent task
	ToolUses   int      `json:"tool_uses,omitempty"`   // tool call count in agent task
	Tokens     int      `json:"tokens,omitempty"`      // total tokens consumed by agent task
	DurationMS int64    `json:"duration_ms,omitempty"` // elapsed ms for agent task
	Status     string   `json:"status,omitempty"`      // agent task status (completed, error, etc.)
	// Agent team internal-view linkage (RFC v4 agent-team-ui §3.2.2).
	// All four fields are persisted to sessions/*.jsonl on "agent" and
	// "task_start" entries so SubagentLinker.SeedFromHistory can rebuild
	// the task_id → on-disk-transcript mapping after shim reconnect or
	// CLI-dead respawn without re-scanning ~/.claude/projects/.
	// Async backfilled via EventLog.SetAgentInternalID once the linker
	// resolves, hence all omitempty.
	TaskType        string `json:"task_type,omitempty"`         // "in_process_teammate" | "local_bash" | ""
	InternalAgentID string `json:"internal_agent_id,omitempty"` // "agent-<hex17>" filename stem under <projectDir>/<sessionID>/subagents/
	JSONLPath       string `json:"jsonl_path,omitempty"`        // absolute path to agent transcript jsonl
	FirstPromptID   string `json:"first_prompt_id,omitempty"`   // jsonl first-line promptId; guards against same-name re-spawn

	// AskQuestion carries the interactive AskUserQuestion card payload. Only
	// set on Type=="ask_question" entries synthesised from an AskUserQuestion
	// tool_use block — kept as a separate field (rather than stuffing JSON
	// into Detail) so the dashboard renderer doesn't have to re-parse and
	// so Go callers (EventLog replay → WS broadcast) don't pay a JSON
	// unmarshal per question bubble.
	AskQuestion *AskQuestion `json:"ask_question,omitempty"`

	// ToolCall is the per-event payload for ACP tool_call / tool_call_update
	// rich progress rows. Multi-Backend RFC §8.3 D17. Same struct on initial
	// invocation and updates; dashboard threads them by ID. Stream-json
	// (Claude) leaves nil and uses Type=="tool_use" with Tool name + Detail
	// for input.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
}

// ToolCall is the per-event payload for ACP tool_call / tool_call_update
// session/update notifications. Multi-Backend RFC §8.3 D17.
//
// Same struct serves both the initial "tool invocation" event (Status==""
// or "pending") and subsequent updates ("in_progress" / "completed" /
// "failed"). The dashboard threads them by ID — successive events with the
// same ID overwrite the prior progress row rather than appending.
//
// Output is the raw JSON payload kiro emits (kiro shape:
// `{"items":[{"Json":{"exit_status":"...", "stdout":"..."}}]}`); the
// dashboard decides how to extract a stdout string vs render JSON. Keeping
// it here as a string preserves the original formatting for "view raw".
type ToolCall struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`        // "execute" / "read" / "write" / "search" / vendor-specific
	Status     string `json:"status,omitempty"`      // "" (initial) / "in_progress" / "completed" / "failed"
	InputJSON  string `json:"input_json,omitempty"`  // raw JSON of rawInput
	OutputJSON string `json:"output_json,omitempty"` // raw JSON of rawOutput
}

// AskQuestion mirrors the shape of AskUserQuestion.input observed against
// claude CLI 2.1.132 (see test/e2e/askuser/aq1_aq2_trigger_and_schema.py).
// ToolUseID is the tool_use id emitted by the assistant and serves as a
// correlation key across dashboard + IM renderings of the same question.
type AskQuestion struct {
	ToolUseID string            `json:"tool_use_id"`
	Items     []AskQuestionItem `json:"items"`
}

// AskQuestionItem is one question in a possibly multi-question card.
// MultiSelect=true signals checkbox semantics; the CLI may set it but the
// dashboard currently degrades to single-select (one click = one answer).
type AskQuestionItem struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
	Options     []AskQuestionOpt `json:"options"`
}

// AskQuestionOpt is one selectable choice. Label is the user-facing text that
// the answer composer will echo back ("Header: Label."). Description is shown
// in the card tooltip / secondary line but never echoed.
type AskQuestionOpt struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
