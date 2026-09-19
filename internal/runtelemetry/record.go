package runtelemetry

// record.go — RunRecord, the one shape "a run happened" takes on every surface
// that leaves the producing subsystem (#2540).
//
// Before this, the same fact wore a different coat per audience: cron and
// sysession each had a private WS frame (cron_run_* / daemon_run_*, four
// frames, two field vocabularies for the same eleven facts), the dashboard
// list APIs each had a private DTO, and session history kept its own terminal
// enum. A consumer that wanted "all runs" had to know every producer. The
// record is the meeting point: producers keep their internal types, but the
// moment a run crosses a wire or an API boundary it is a RunRecord.

import "time"

// RunRecord is the subsystem-neutral description of one run's lifecycle.
// OwnerID is the producer-side identity of the run target — a cron job ID, a
// sysession daemon name — with the character-domain contract documented on
// RunStartedEvent.OwnerID.
type RunRecord struct {
	Subsystem  Subsystem   `json:"subsystem"`
	OwnerID    string      `json:"owner_id"`
	RunID      string      `json:"run_id"`
	State      RunState    `json:"state,omitempty"` // empty until terminal
	Trigger    TriggerKind `json:"trigger,omitempty"`
	StartedAt  time.Time   `json:"started_at"`
	EndedAt    time.Time   `json:"ended_at,omitzero"`
	DurationMS int64       `json:"duration_ms,omitempty"`
	SessionID  string      `json:"session_id,omitempty"`
	ErrorClass ErrorClass  `json:"error_class,omitempty"`
	// ErrorMsg is server-side-only by default; see RunEndedEvent's SECURITY
	// note. A boundary that forwards a RunRecord off-host decides per
	// subsystem whether this field survives.
	ErrorMsg string `json:"error_msg,omitempty"`
	// Fresh is cron-specific: the session was Reset before spawn.
	Fresh bool `json:"fresh,omitempty"`
	// CostUSD is filled by producers that account per-run cost (session runs
	// today; cron after the cost-ledger integration). Zero means unknown, not
	// free — see #2750 for why an unknown cost must never be guessed.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Record projects the started event onto the common record. One derivation:
// the WS broadcaster and any future consumer read the same projection, so a
// field added to the event cannot reach one surface and miss another.
func (ev RunStartedEvent) Record() RunRecord {
	return RunRecord{
		Subsystem: ev.Subsystem,
		OwnerID:   ev.OwnerID,
		RunID:     ev.RunID,
		Trigger:   ev.Trigger,
		StartedAt: ev.StartedAt,
		SessionID: ev.SessionID,
		Fresh:     ev.Fresh,
	}
}

// Record projects the ended event onto the common record.
func (ev RunEndedEvent) Record() RunRecord {
	return RunRecord{
		Subsystem:  ev.Subsystem,
		OwnerID:    ev.OwnerID,
		RunID:      ev.RunID,
		State:      ev.State,
		Trigger:    ev.Trigger,
		StartedAt:  ev.StartedAt,
		EndedAt:    ev.EndedAt,
		DurationMS: ev.DurationMS,
		SessionID:  ev.SessionID,
		ErrorClass: ev.ErrorClass,
		ErrorMsg:   ev.ErrorMsg,
	}
}
