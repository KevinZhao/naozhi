// Package runview holds the one JSON shape a run-history row takes on every
// dashboard list API (#2540). Before it, the same eleven facts wore a
// different coat per producer: cron rows spoke state, session rows spoke
// outcome (a four-value vocabulary nothing else used), and a client rendering
// "a run" had to know who ran it. The shape is the JSON projection of
// runtelemetry.RunRecord — unix-ms timestamps because every consumer is
// dashboard JS — plus the per-producer extras (replay_of, first_byte_ms) that
// are meaningless outside their subsystem but harmless as omitempty.
package runview

import (
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// Summary is one run-history row. State always speaks runtelemetry.RunState
// (succeeded / failed / skipped / timed_out / canceled); producers whose
// internal vocabulary differs map at this boundary and nowhere else.
type Summary struct {
	Subsystem  string `json:"subsystem,omitempty"`
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	Trigger    string `json:"trigger,omitempty"`
	StartedAt  int64  `json:"started_at"`
	EndedAt    int64  `json:"ended_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	// ReplayOf links a cron replay run to its origin (agentcore §7.3).
	ReplayOf string `json:"replay_of,omitempty"`
	// FirstByteMS is session-run latency instrumentation.
	FirstByteMS int64   `json:"first_byte_ms,omitempty"`
	CostUSD     float64 `json:"cost_usd,omitempty"`
}

// FromSessionRun projects a persisted session run onto the common row. The
// on-disk record keeps its outcome word (rewriting history files is not this
// package's business); the wire stops speaking it here.
func FromSessionRun(r runhistory.SessionRun) Summary {
	v := Summary{
		Subsystem:   string(runtelemetry.SubsystemSession),
		RunID:       r.RunID,
		State:       string(r.Outcome.RunState()),
		StartedAt:   r.StartedAt.UnixMilli(),
		DurationMS:  r.DurationMS,
		FirstByteMS: r.FirstByteMS,
		CostUSD:     r.CostUSD,
		ErrorClass:  string(r.ErrorClass),
	}
	if !r.EndedAt.IsZero() {
		v.EndedAt = r.EndedAt.UnixMilli()
	}
	return v
}

// MSTime converts for producers that project by hand (cron keeps its own
// projection because CronRunSummary carries sanitisation the row needs).
func MSTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
