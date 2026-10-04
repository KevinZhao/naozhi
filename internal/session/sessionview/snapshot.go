package sessionview

import (
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
	"github.com/naozhi/naozhi/internal/spawndiag"
)

// SessionSnapshot is a point-in-time view of a session for the dashboard API.
type SessionSnapshot struct {
	Key        string `json:"key"`
	Platform   string `json:"platform"`
	Agent      string `json:"agent"`
	SessionID  string `json:"session_id"`
	State      string `json:"state"`
	Protocol   string `json:"protocol"`
	Backend    string `json:"backend,omitempty"`     // "claude", "kiro", ...
	CLIName    string `json:"cli_name,omitempty"`    // "claude-code", "kiro"
	CLIVersion string `json:"cli_version,omitempty"` // e.g. "2.1.92"
	// AccessProfile is the access-profile ID this session spawned under
	// ("" = global default). Label/colour live in the /api/access-profiles
	// registry, NOT here — the snapshot never carries env values or secrets
	// (RFC project-access-profile §8.3).
	AccessProfile string `json:"access_profile,omitempty"`
	// Model is the CLI model identifier (live process value, else the
	// persisted spawn-time value). Empty when the operator did not configure
	// one; the dashboard renders "(模型未配置)". For ACP backends the runtime
	// model from session/new is not read back (see docs/TODO.md), so this
	// reflects the configured value.
	Model      string `json:"model,omitempty"`
	LastActive int64  `json:"last_active"` // unix ms
	// CreatedAt anchors sidebar order (ascending, so new rows land at the
	// bottom and rows never shift on activity). unix ms; 0 only if loadStore
	// couldn't infer one (treated as "very old").
	CreatedAt    int64   `json:"created_at,omitempty"`
	TotalCost    float64 `json:"total_cost"`
	Workspace    string  `json:"workspace,omitempty"`
	DeathReason  string  `json:"death_reason,omitempty"`
	DeathDetail  string  `json:"death_detail,omitempty"` // stderr line naming a non-zero exit's cause
	ChatType     string  `json:"chat_type,omitempty"`
	ChatID       string  `json:"chat_id,omitempty"`
	Node         string  `json:"node,omitempty"`
	LastPrompt   string  `json:"last_prompt,omitempty"`   // most recent user message
	LastActivity string  `json:"last_activity,omitempty"` // most recent tool/thinking status
	// LastResponse is the truncated summary of the most recent assistant text
	// reply for the sidebar preview: live proc.LastResponseSummary, falling
	// back to the s.lastResponse cache for suspended/dead sessions.
	LastResponse string `json:"last_response,omitempty"`
	Summary      string `json:"summary,omitempty"`    // Claude-generated session title
	UserLabel    string `json:"user_label,omitempty"` // operator-set override for sidebar/header title
	// LabelOrigin records who set UserLabel: "" / "user" (human) or "auto"
	// (sysession daemon); drives the bot icon and "restore auto naming"
	// action (docs/rfc/system-session.md §7.3 / §9.3).
	LabelOrigin     string              `json:"label_origin,omitempty"`
	Project         string              `json:"project,omitempty"`          // project name (filled by server)
	ProjectFallback bool                `json:"project_fallback,omitempty"` // true when Project is a workspace-basename fallback, not a registered project
	IsPlanner       bool                `json:"is_planner,omitempty"`       // true for project planner sessions
	Subagents       []ring.SubagentInfo `json:"subagents,omitempty"`        // active sub-agent types in current turn
	// MessageCount is the cumulative "user" turn count: from the live Process
	// event log since spawn, else the persistedHistory count. Not persisted;
	// InjectHistory → EventLog.AppendBatch rebuilds it on reconnect.
	MessageCount int64 `json:"message_count,omitempty"`

	// Normalized cross-backend status fields (docs/rfc/multi-backend.md §8.8)
	// so dashboard / IM / cron never parse backend-private events.
	//
	// CostUnit is "USD" for claude-class backends and the backend-reported
	// unit for ACP-class (kiro: "credits"). Empty when no known backend.
	CostUnit string `json:"cost_unit,omitempty"`
	// ContextUsagePercent is 0-100 context utilisation (kiro only; claude 0).
	ContextUsagePercent float64 `json:"context_usage_percent,omitempty"`
	// TurnDurationMs is the last completed turn's duration (kiro only; claude 0).
	TurnDurationMs int64 `json:"turn_duration_ms,omitempty"`
	// MeteringUsage carries backend-reported per-turn billing rows (kiro).
	// READ-ONLY, shared across snapshots: while MeteringGen is unchanged
	// every Snapshot returns the same backing array (#2345). Consumers,
	// SnapshotEnricher hooks included, must copy before mutating.
	MeteringUsage []clievent.MeteringEntry `json:"metering_usage,omitempty"`
	// Effort is the backend's thinking-effort tier for the latest turn
	// (low/medium/high/xhigh/max on kiro). Empty for backends that report
	// none, evicted sessions, and before the first metadata frame; the
	// dashboard hides the tag. Not persisted, so it resets across restarts
	// (docs/rfc/kiro-effort-visibility.md).
	Effort string `json:"effort,omitempty"`
	// StartupFailure is what the next send to a dead session will do about
	// its CLI's failures at startup; nil when it just resumes.
	StartupFailure *StartupFailureView `json:"startup_failure,omitempty"`
	// SpawnDiags is what the spawn gates dropped/ignored for the live process
	// (#2532). Always serialised — an empty array, not undefined, so the
	// dashboard can index it unconditionally. Runtime observation like
	// Effort: empty for evicted sessions and across restarts.
	SpawnDiags []spawndiag.Diag `json:"spawn_diags"`
	// OverlayDrift lists the argv-bearing fields whose live value differs
	// from what a fresh spawn under the current config would use (#2543);
	// remedy is restarting the session. Same always-an-array contract as
	// SpawnDiags.
	OverlayDrift []OverlayFieldDrift `json:"overlay_drift"`
}

// StartupFailureView is a dead session's run of CLI startup failures, for the
// dashboard's exit chip.
type StartupFailureView struct {
	// Class is the latest failure's clierr.ExitClass wire name, Streak the
	// failures in a row; both empty when only NewSession is set.
	Class  string `json:"class,omitempty"`
	Streak int32  `json:"streak,omitempty"`
	// RetryAt is when the startup breaker lets the next spawn run (unix ms);
	// 0 when it does not pause the key.
	RetryAt int64 `json:"retry_at,omitempty"`
	// NewSession: the next send starts a new conversation, not the resume.
	NewSession bool `json:"new_session,omitempty"`
}

// OverlayFieldDrift is one argv-bearing field whose live value (the argv the
// surviving shim was spawned with) differs from what a fresh spawn under the
// CURRENT config would use (#2543). Surfaced per session on /api/sessions as
// overlay_drift; the remedy is restarting the session — a live session is
// never auto-restarted over drift.
type OverlayFieldDrift struct {
	// Field is "model" | "effort" | "append_system_prompt", or "args" when
	// the argv differs without any of the named tokens differing (codex-class
	// backends render model/effort without dedicated flags).
	Field string `json:"field"`
	// Stored is the value in the shim's recorded argv ("" = absent).
	Stored string `json:"stored"`
	// Current is the value a fresh spawn would use now ("" = absent).
	Current string `json:"current"`
}
