// scheduler_inflight.go: cron dispatch entry points + per-job inflight
// (runInflight) bookkeeping.

package cron

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// executeIfNotDeletedOrPaused is the TriggerNow dispatch entry: snapshot the
// freshest *Job under s.mu.RLock, then — only if still present and not
// paused — release the lock and call executeOpt(cur, true). Deleted or paused
// jobs surface as a Debug-log skip with no run record.
//
// LOCK: caller MUST NOT hold s.mu; the snapshot → release → executeOpt split
// keeps executeOpt's long-running send/notify pipeline off s.mu. TriggerNow's
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
// (executeIfNotDeletedOrPaused) and the registerJob AddFunc closure; only the
// viaTriggerNow flag and the skip-log subject ("TriggerNow:" vs "cron:") differ.
func (s *Scheduler) executeJobIDIfLive(jobID string, viaTriggerNow bool, logSubject string) {
	s.mu.RLock()
	cur, ok := s.jobs[jobID]
	paused := ok && cur.Paused
	s.mu.RUnlock()
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

// cleanupRunningJobIfIdle drops the s.runningJobs entry for jobID iff the
// runInflight CAS gate is currently false (no in-flight execute() holds it),
// so a deployment that adds and deletes thousands of jobs does not accumulate
// dead *runInflight structs (#758). If the gate is held the entry is left alone
// — the executeOpt goroutine still holds the pointer and is about to
// releaseRun(); the leak is bounded by jobs deleted mid-run.
//
// Returns true if the entry was deleted. Safe to call after s.mu is released —
// sync.Map needs no scheduler lock; callers run it from lock-free postCleanup
// branches.
func (s *Scheduler) cleanupRunningJobIfIdle(jobID string) bool {
	// Take the per-jobID gate around the whole Load → running-check →
	// CompareAndDelete sequence so it is atomic relative to executeOpt's
	// jobInflight-load→CAS pair, which holds the same gate (#1706).
	gate := s.jobGateLock(jobID)
	gate.Lock()
	defer gate.Unlock()

	v, ok := s.runningJobs.Load(jobID)
	if !ok {
		return false
	}
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		// Package invariant violated upstream; log loud (#1392). CompareAndDelete
		// on the observed v (not LoadAndDelete on the key) so a concurrent
		// jobInflight that already replaced this stale entry is not collateral.
		slog.Error("cron: runningJobs holds unexpected value type; sweeping",
			"job_id", jobID, "type", fmt.Sprintf("%T", v))
		s.runningJobs.CompareAndDelete(jobID, v)
		return true
	}
	if inf.running.Load() {
		// In-flight execute() still holds the pointer and is about to
		// releaseRun(); leaking this one entry until the next DeleteJob sweep
		// is cheaper than risking a CAS-gate split.
		return false
	}
	// CompareAndDelete on OUR observed inf pointer (not LoadAndDelete on the
	// key) so a fresh *runInflight stored by a racing AddJob+jobInflight is left
	// alone (#1416). The residual race — executeOpt did
	// `inflight := s.jobInflight(j.ID)` then CAS-won on the orphaned old gate after
	// we deleted it — is closed by the per-jobID gate held above (#1706).
	s.runningJobs.CompareAndDelete(jobID, inf)
	return true
}

// jobInflight returns a lazily created *runInflight per job ID. The embedded
// atomic.Bool is the CAS gate executeOpt uses to reject concurrent runs; the
// surrounding metadata (RunID/StartedAt/Phase) feeds the list API. Entries
// are reclaimed on DeleteJob via cleanupRunningJobIfIdle when the gate is idle.
func (s *Scheduler) jobInflight(id string) *runInflight {
	if v, ok := s.runningJobs.Load(id); ok {
		if inf, ok := v.(*runInflight); ok && inf != nil {
			return inf
		}
	}
	guard := &runInflight{}
	actual, _ := s.runningJobs.LoadOrStore(id, guard)
	if inf, ok := actual.(*runInflight); ok && inf != nil {
		return inf
	}
	// Should be unreachable given LoadOrStore's contract, but never return
	// nil to callers — they immediately call methods on the result.
	return guard
}

// rangeRunningSessionIDs invokes fn for the Claude session ID of every
// currently-running inflight run (a run whose SessionID has been populated by
// setSessionID after GetOrCreate). fn returning false stops the iteration
// early — like sync.Map.Range — so a caller searching for one ID can bail on
// the first hit. Empty SessionIDs (run started but session not yet minted)
// and non-running snapshots are skipped before fn sees them.
func (s *Scheduler) rangeRunningSessionIDs(fn func(sessionID string) bool) {
	s.runningJobs.Range(func(_, v any) bool {
		inf, ok := v.(*runInflight)
		if !ok || inf == nil {
			return true
		}
		view, running := inf.snapshot()
		if !running || view.SessionID == "" {
			return true
		}
		return fn(view.SessionID)
	})
}
