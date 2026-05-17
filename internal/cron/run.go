package cron

import "time"

// CronRun is the persistent record of a single cron job execution.
// Lifecycle:
//
//   - Created in-memory at executeOpt CAS gate (runInflight 持有 RunID/
//     StartedAt/Phase/Trigger，但不写盘).
//   - On terminal transition, finishRun calls runStore.Append to write
//     a single <runs/<jobID>/<run_id>.json> file under storage root, and
//     updates index.json. Skipped runs whose skipPersist=true do NOT
//     persist (overlap_skipped / canceled / paused_concurrent) — see
//     RFC §4.2 for the rationale (avoid noise, transient by definition).
//   - List / detail handlers read it back via runStore.List / Get.
//   - GC trims to (max 200 条 AND 30 天) per job.
//
// Field choices vs. Job:
//
//   - Prompt / WorkDir / Fresh are SNAPSHOT at execute time, not the
//     current Job values. This prevents Prompt drift: a user editing
//     Job.Prompt mid-history must still be able to see "what prompt
//     produced run X's result".
//   - SessionID is the Claude session_id this run produced. fresh=false
//     mode: same value across many CronRuns (all sharing one JSONL).
//     fresh=true mode: each CronRun has a unique SessionID linking to
//     its own JSONL file.
//   - Result is rune-truncated to 4K (matches recordResultP0 path); the
//     full Send result text is not preserved (it lives only in the
//     session's persistent event log + JSONL).
//   - ErrorMsg is path-redacted + sanitized via the same pipeline that
//     populates Job.LastError so dashboard rendering does not need a
//     second sanitization step.
//
// Wire shape: every JSON tag has an explicit name. omitempty applies to
// fields whose zero value is meaningless (Result for failures, ErrorMsg
// for successes, EndedAt for the legacy "running" snapshot which is
// never persisted but the field stays present for symmetric tooling).
type CronRun struct {
	RunID      string      `json:"run_id"`
	JobID      string      `json:"job_id"`
	State      RunState    `json:"state"`
	Trigger    TriggerKind `json:"trigger,omitempty"`
	StartedAt  time.Time   `json:"started_at"`
	EndedAt    time.Time   `json:"ended_at,omitempty"`
	DurationMS int64       `json:"duration_ms,omitempty"`

	// SessionID 在 fresh=true 路径下每条 run 独有，用来定位 ~/.claude/
	// projects/<cwd>/<session_id>.jsonl；fresh=false 路径下多条 run 共享
	// 同一 SessionID（详见 docs/rfc/cron-run-history.md §2）。
	SessionID string `json:"session_id,omitempty"`

	Prompt  string `json:"prompt,omitempty"`
	WorkDir string `json:"work_dir,omitempty"`
	Fresh   bool   `json:"fresh,omitempty"`

	Result      string     `json:"result,omitempty"`
	ResultBytes int        `json:"result_bytes,omitempty"`
	ErrorClass  ErrorClass `json:"error_class,omitempty"`
	ErrorMsg    string     `json:"error_msg,omitempty"`
}

// CronRunSummary is the slim shape returned by list endpoints + the
// recent_runs field on the cron list view. Drops Prompt / Result / full
// ErrorMsg so a /api/cron page with 50 jobs × 5 recent_runs does not
// inflate to multi-MB. Detail endpoint returns full CronRun.
type CronRunSummary struct {
	RunID      string      `json:"run_id"`
	JobID      string      `json:"job_id,omitempty"` // omitted in per-job nested context
	State      RunState    `json:"state"`
	Trigger    TriggerKind `json:"trigger,omitempty"`
	StartedAt  time.Time   `json:"started_at"`
	EndedAt    time.Time   `json:"ended_at,omitempty"`
	DurationMS int64       `json:"duration_ms,omitempty"`
	SessionID  string      `json:"session_id,omitempty"`
	ErrorClass ErrorClass  `json:"error_class,omitempty"`
}

// summary derives a CronRunSummary from a CronRun. Centralised so any
// future field addition stays in lockstep across list endpoint, recent_runs
// nested array, and any test fixtures.
func (r *CronRun) summary() CronRunSummary {
	return CronRunSummary{
		RunID:      r.RunID,
		JobID:      r.JobID,
		State:      r.State,
		Trigger:    r.Trigger,
		StartedAt:  r.StartedAt,
		EndedAt:    r.EndedAt,
		DurationMS: r.DurationMS,
		SessionID:  r.SessionID,
		ErrorClass: r.ErrorClass,
	}
}
