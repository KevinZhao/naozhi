package runstore

import (
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// CronRun is the persistent record of a single cron job execution. It is
// created in memory at executeOpt's CAS gate and written once by cron's finishRun via
// Store.Append to runs/<jobID>/<run_id>.json on the terminal transition;
// skipPersist runs (overlap_skipped / canceled / paused_concurrent) are not
// persisted. GC trims to (keepCount AND keepWindow) per job.
//
// Prompt / WorkDir / Fresh are SNAPSHOTS at execute time so editing Job.Prompt
// never changes what a past run shows. SessionID is shared across runs when
// fresh=false and unique per run when fresh=true. Result is rune-truncated;
// ErrorMsg is already redacted + sanitized like Job.LastError.
type CronRun struct {
	RunID      string                   `json:"run_id"`
	JobID      string                   `json:"job_id"`
	State      runtelemetry.RunState    `json:"state"`
	Trigger    runtelemetry.TriggerKind `json:"trigger,omitempty"`
	StartedAt  time.Time                `json:"started_at"`
	EndedAt    time.Time                `json:"ended_at"` // omitempty has no effect on time.Time; use IsZero to test emptiness
	DurationMS int64                    `json:"duration_ms,omitempty"`

	// SessionID 在 fresh=true 路径下每条 run 独有，用来定位 ~/.claude/
	// projects/<cwd>/<session_id>.jsonl；fresh=false 路径下多条 run 共享
	// 同一 SessionID（详见 docs/rfc/cron-run-history.md §2）。
	SessionID string `json:"session_id,omitempty"`

	Prompt  string `json:"prompt,omitempty"`
	WorkDir string `json:"work_dir,omitempty"`
	Fresh   bool   `json:"fresh,omitempty"`

	Result      string                  `json:"result,omitempty"`
	ResultBytes int                     `json:"result_bytes,omitempty"`
	ErrorClass  runtelemetry.ErrorClass `json:"error_class,omitempty"`
	ErrorMsg    string                  `json:"error_msg,omitempty"`

	// CostUSD is the run's spend increment: what was booked while the run's
	// Send was live (cost window, docs/rfc/cost-ledger.md §5.0).
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// CronRunSummary is the slim shape returned by list endpoints + the
// recent_runs field on the cron list view. Drops Prompt / Result / full
// ErrorMsg so a /api/cron page with 50 jobs × 5 recent_runs does not
// inflate to multi-MB. Detail endpoint returns full CronRun.
type CronRunSummary struct {
	RunID      string                   `json:"run_id"`
	JobID      string                   `json:"job_id,omitempty"` // omitted in per-job nested context
	State      runtelemetry.RunState    `json:"state"`
	Trigger    runtelemetry.TriggerKind `json:"trigger,omitempty"`
	StartedAt  time.Time                `json:"started_at"`
	EndedAt    time.Time                `json:"ended_at"` // omitempty has no effect on time.Time; use IsZero to test emptiness
	DurationMS int64                    `json:"duration_ms,omitempty"`
	SessionID  string                   `json:"session_id,omitempty"`
	ErrorClass runtelemetry.ErrorClass  `json:"error_class,omitempty"`
	// CostUSD is carried in the slim summary so the per-run cost and per-job
	// monthly aggregate (front-end sum over recent_runs) need no detail fetch.
	// 0/omitted for runs that produced no cost.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// summary derives a CronRunSummary from a CronRun. Centralised so any
// future field addition stays in lockstep across list endpoint, recent_runs
// nested array, and any test fixtures.
func (r *CronRun) summary() CronRunSummary {
	s := CronRunSummary{
		RunID:      r.RunID,
		JobID:      r.JobID,
		State:      r.State,
		Trigger:    r.Trigger,
		StartedAt:  r.StartedAt,
		EndedAt:    r.EndedAt,
		DurationMS: r.DurationMS,
		SessionID:  r.SessionID,
		ErrorClass: r.ErrorClass,
		// Monthly aggregates sum this off recent_runs (#2280).
		CostUSD: r.CostUSD,
	}
	return s
}
