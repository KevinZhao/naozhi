// scheduler_inflight.go: cron dispatch entry points + per-job inflight
// (runInflight) bookkeeping.

package cron

import (
	"log/slog"
	"runtime/debug"
)

// executeIfNotDeletedOrPaused is the TriggerNow dispatch entry: snapshot the
// freshest *Job under s.tbl.mu.RLock, then — only if still present and not
// paused — release the lock and call executeOpt(cur, true). Deleted or paused
// jobs surface as a Debug-log skip with no run record.
//
// LOCK: caller MUST NOT hold s.tbl.mu; the snapshot → release → executeOpt split
// keeps executeOpt's long-running send/notify pipeline off s.tbl.mu. TriggerNow's
// goroutine bypasses robfig/cron's Recover wrapper, so recover here (#801);
// the scheduled tick routes through executeJobIDIfLive directly to avoid
// double-recovering.
func (s *Scheduler) executeIfNotDeletedOrPaused(jobID string) {
	defer func() {
		if r := recover(); r != nil {
			recordTriggerNowPanic(jobID, r)
		}
	}()
	s.executeJobIDIfLive(jobID, true /* viaTriggerNow */, "TriggerNow")
}

// recordTriggerNowPanic logs a TriggerNow-path panic; split out so the recover
// site stays a one-liner and the log path is testable.
func recordTriggerNowPanic(jobID string, r any) {
	slog.Error("TriggerNow: panic recovered, run abandoned",
		"job_id", jobID,
		"panic", r,
		"stack", string(debug.Stack()))
}

// executeJobIDIfLive is the shared lookup-and-dispatch primitive for TriggerNow
// (executeIfNotDeletedOrPaused) and the registered tick closure; only the
// viaTriggerNow flag and the skip-log subject ("TriggerNow:" vs "cron:") differ.
func (s *Scheduler) executeJobIDIfLive(jobID string, viaTriggerNow bool, logSubject string) {
	// NOT s.liveness(jobID): executeOpt takes the live *Job, so the pointer has to
	// escape this critical section. That is the one registry escape hatch left in
	// production, and closing it means giving executeOpt a snapshot instead — a
	// change to the run pipeline, not to the registry.
	s.tbl.mu.RLock()
	cur, ok := s.tbl.jobs[jobID]
	paused := ok && cur.Paused
	s.tbl.mu.RUnlock()
	// slog.With is built lazily (skip path only) to avoid ~500 wasted
	// allocs/sec on the hot live-job path.
	if !ok || paused {
		lg := slog.With("subject", logSubject, "job_id", jobID)
		if !ok {
			lg.Debug("job deleted before execute, skipping")
		} else {
			lg.Debug("job paused concurrently, skipping")
		}
		return
	}
	s.executeOpt(cur, viaTriggerNow)
}

// rangeRunningSessionIDs invokes fn for the Claude session ID of every
// currently-running inflight run (a run whose SessionID has been populated by
// setSessionID after GetOrCreate). fn returning false stops the iteration
// early — like sync.Map.Range — so a caller searching for one ID can bail on
// the first hit. Empty SessionIDs (run started but session not yet minted)
// and non-running snapshots are skipped before fn sees them.
func (s *Scheduler) rangeRunningSessionIDs(fn func(sessionID string) bool) {
	s.gate.rangeInflight(func(inf *runInflight) bool {
		view, running := inf.snapshot()
		if !running || view.SessionID == "" {
			return true
		}
		return fn(view.SessionID)
	})
}
