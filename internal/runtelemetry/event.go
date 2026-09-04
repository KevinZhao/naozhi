package runtelemetry

import "time"

// RunStartedEvent fires after a producer takes its per-run inflight CAS
// gate, before any long-running IO. Producers MUST emit this exactly once
// per RunID.
type RunStartedEvent struct {
	Subsystem Subsystem

	// OwnerID is the producer-side identity of the run target. Character
	// domain per Subsystem: SubsystemCron => 16-char lowercase hex (trusted;
	// broadcaster shape-validates); SubsystemSysession => compiled-in
	// builtinDaemons name (broadcaster applies osutil.SanitizeForLog). A new
	// Subsystem MUST extend this contract before its broadcast branch lands.
	OwnerID string

	// RunID is a producer-generated 16-char hex ID, paired 1:1 with a RunEndedEvent.
	RunID string

	Trigger   TriggerKind
	StartedAt time.Time

	// SessionID may be empty when the started frame is broadcast before
	// session.GetOrCreate has resolved (cron's normal case).
	SessionID string

	// Fresh is cron-specific: the session was Reset before spawn. Always false for sysession.
	Fresh bool
}

// RunEndedEvent fires when a run reaches a terminal RunState. Producers
// MUST emit this exactly once per RunID, paired with the RunStartedEvent.
//
// SECURITY: ErrorMsg is server-side-only; the broadcaster decides whether it
// goes on the WS wire. cron passes it through (already redacted +
// SanitizeForLog'd); sysession drops it, because daemon errors can echo
// prompt fragments and broadcasting them to every dashboard client would
// leak conversation excerpts (docs/rfc/system-session.md §9.4).
type RunEndedEvent struct {
	Subsystem  Subsystem
	OwnerID    string
	RunID      string
	State      RunState
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMS int64
	Trigger    TriggerKind

	SessionID  string
	ErrorClass ErrorClass
	ErrorMsg   string
}
