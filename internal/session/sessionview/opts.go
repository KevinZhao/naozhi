package sessionview

// AgentOpts provides per-agent overrides for session creation.
//
// ExtraArgs aliasing contract: callers receiving AgentOpts from KeyResolver
// get a freshly-cloned ExtraArgs (safe to append). Callers populating
// AgentOpts must own ExtraArgs exclusively — do NOT alias slices held by
// other goroutines.
type AgentOpts struct {
	Model     string
	ExtraArgs []string
	Workspace string // override workspace (empty = use default/chat override)
	Backend   string // backend ID ("claude" / "kiro" / …); empty = router default
	// DefaultBackend is the agent-config backend (agents[].backend). It ranks
	// below Backend, the dashboard pick and the dead session's backend, and
	// above the router default, so it neither overrides the picker nor moves
	// a resumable session onto another CLI. Empty = router default.
	DefaultBackend string
	// AccessProfile names the access profile (auth/upstream env overlay +
	// default model) to spawn under. Empty = global default. Resume
	// continuity takes precedence over the caller's value: a dead session
	// must resume on the SAME auth chain it was created on (RFC
	// project-access-profile §7).
	AccessProfile string
	// Effort overrides the backend's thinking-effort tier for this session.
	// Empty = inherit. Only ACP-protocol backends act on it.
	Effort string
	// SystemPrompt is the text appended to the CLI's system prompt
	// (`--append-system-prompt` via cli.SpawnOptions.AppendSystemPrompt).
	// Layers (agents[<id>].system_prompt → planner prompt → scratch context)
	// are pre-joined by JoinSystemPrompts into this one string. Empty = no
	// flag. Never put the flag into ExtraArgs instead — it is denylisted
	// there and silently stripped (#2493). Only the Claude backend renders it.
	SystemPrompt string
	Exempt       bool // exempt from TTL, eviction, and activeCount (planner sessions)
}

// SessionStatus indicates how a session was obtained.
type SessionStatus int

const (
	SessionExisting SessionStatus = iota // reused a live session
	SessionResumed                       // resumed a suspended session
	SessionNew                           // created a brand new session
	// SessionResumeLost: a suspended session with a session ID whose resume
	// target was missing or invalid, so it was spawned fresh and the
	// conversation's context is gone.
	SessionResumeLost
)
