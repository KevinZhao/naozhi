package runtelemetry

import "time"

// RunStartedEvent fires after a producer takes its per-run inflight CAS
// gate, before any long-running IO. Producers MUST emit this exactly
// once per RunID.
//
// OwnerID character domain depends on Subsystem and is documented on the
// field; broadcaster implementations select the right sanitiser per
// Subsystem.
type RunStartedEvent struct {
	Subsystem Subsystem

	// OwnerID is the producer-side identity of the run target. Its
	// character domain depends on Subsystem:
	//
	//   SubsystemCron      => 16-char lowercase hex (cron.generateHexID).
	//                         Trusted; broadcaster uses
	//                         sanitizeHexIDForBroadcast for cheap
	//                         shape-validation.
	//   SubsystemSysession => builtinDaemons registered name (compiled
	//                         in, not user input). Broadcaster uses
	//                         osutil.SanitizeForLog as defence-in-depth.
	//
	// A future Subsystem (planner / system) MUST extend this contract
	// before its broadcast branch lands, otherwise the broadcaster has
	// no defined sanitiser for the new shape.
	OwnerID string

	// RunID is a producer-generated 16-char hex correlation ID. Pairs
	// 1:1 with a subsequent RunEndedEvent for the same run.
	RunID string

	Trigger   TriggerKind
	StartedAt time.Time

	// SessionID may be empty when the producer broadcasts the started
	// frame before its session.GetOrCreate has resolved (cron's normal
	// case: started fires post-CAS, GetOrCreate runs after).
	SessionID string

	// Fresh is cron-specific: indicates this run is in fresh-context mode
	// (session was Reset before spawn). Always false for sysession.
	Fresh bool
}

// RunEndedEvent fires when a run reaches a terminal RunState. Producers
// MUST emit this exactly once per RunID, paired with the matching
// RunStartedEvent.
//
// SECURITY: ErrorMsg is server-side-only. Whether to include it on the
// WS wire is the broadcaster's responsibility:
//
//   - cron: emits ErrorMsg post-redactPathsInCronError + post-SanitizeForLog
//     pipeline (recordResultP0WithSanitised); broadcaster passes through.
//   - sysession: deliberately drops ErrorMsg before serialising — daemon
//     errors can echo prompt fragments back from the LLM subprocess and
//     broadcasting that to every authenticated dashboard client would
//     leak conversation excerpts cross-tenant. See
//     docs/rfc/system-session.md §9.4 / Sec-LOW-2.
//
// runtelemetry itself does not enforce a policy on ErrorMsg — producers
// pass whatever they have, broadcasters decide what to put on the wire.
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
