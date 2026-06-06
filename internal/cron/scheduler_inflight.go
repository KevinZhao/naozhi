// scheduler_inflight.go: cron dispatch entry points + per-job inflight
// (runInflight) bookkeeping.
//
// Split out of scheduler_run.go (move-only, no behaviour change): the
// TriggerNow / scheduled dispatch helpers (executeIfNotDeletedOrPaused /
// executeJobIDIfLive) plus the runInflight lifecycle (cleanupRunningJobIfIdle
// / jobInflight / rangeRunningSessionIDs). None read s.stopCtx; the per-jobID
// CAS gate + cleanup atomicity contract is preserved verbatim. Methods stay on
// *Scheduler so private fields remain accessible without exporting.

package cron

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// executeIfNotDeletedOrPaused is the TriggerNow dispatch entry. It looks
// up the freshest *Job under s.mu.RLock, then — only if still present and
// not paused — releases the lock and calls executeOpt(cur, true). Deleted
// or paused jobs surface as a Debug-log skip with no run record.
//
// LOCK: caller MUST NOT hold s.mu (this acquires s.mu.RLock); robfig/cron
// MUST NOT hold its internal cron lock when invoking this (the snapshot →
// release → executeOpt split exists precisely so executeOpt's long-running
// send/notify pipeline never runs under s.mu).
//
// R238-GO-9 (#801): TriggerNow's goroutine bypasses the robfig/cron chain's
// Recover wrapper that protects the scheduled-tick path, so a panic in
// executeOpt would propagate up to the TriggerNow goroutine and kill it
// (and any inflight defer that hadn't fired yet — the deferred Done in
// scheduler_jobs.go's TriggerNow closure DOES still fire because it's
// registered before this call, but the panic surfaces as a runtime crash
// in the slog of the goroutine that ran it). Recover here so a panicking
// job fails loud once (Error log + stack) and the surrounding goroutine
// still completes. The scheduled path keeps robfig's Recover and does NOT
// pass through this helper — registerJob's AddFunc closure routes through
// executeJobIDIfLive directly so we don't double-recover.
func (s *Scheduler) executeIfNotDeletedOrPaused(jobID string) {
	defer func() {
		if r := recover(); r != nil {
			recordTriggerNowPanic(jobID, r)
		}
	}()
	s.executeJobIDIfLive(jobID, true /* viaTriggerNow */, "TriggerNow")
}

// recordTriggerNowPanic logs a TriggerNow-path panic. Split out of
// executeIfNotDeletedOrPaused's defer so the recover site stays a one-
// liner and the formatted-log path is exercisable in tests without
// deferring inside the test body. R238-GO-9 (#801).
func recordTriggerNowPanic(jobID string, r any) {
	slog.Error("TriggerNow: panic recovered, run abandoned",
		"job_id", jobID,
		"panic", r,
		"stack", string(debug.Stack()))
}

// executeJobIDIfLive is the shared lookup-and-dispatch primitive used by
// both TriggerNow (executeIfNotDeletedOrPaused) and the registerJob
// AddFunc closure (R247-CR-10). Both paths previously open-coded the
// RLock → exists/paused check → executeOpt fan-out with only the
// viaTriggerNow flag and Debug log subject differing; the duplicated
// closure made it easy to drift one path's pre-flight gate without the
// other. logSubject is the caller-supplied prefix used in skip Debug
// logs so operators distinguish "TriggerNow:" vs "cron:" in the
// shutdown / pause race traces.
func (s *Scheduler) executeJobIDIfLive(jobID string, viaTriggerNow bool, logSubject string) {
	s.mu.RLock()
	cur, ok := s.jobs[jobID]
	paused := ok && cur.Paused
	s.mu.RUnlock()
	// R243-ARCH-13 (#841): bind the {subject, job_id} label pair once via
	// slog.With instead of re-listing the same two keys at every skip-log
	// site. Keeps the two skip-branch Debug lines from drifting their label
	// set apart. Constructed lazily (only on skip path) to avoid ~500
	// wasted allocs/sec on the hot live-job path (R093146-PERF-1).
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

// cleanupRunningJobIfIdle drops the s.runningJobs entry for jobID iff
// the runInflight CAS gate is currently false (no in-flight execute()
// holds it). R242-ARCH-15 (#758): the prior policy was "never clean,
// bounded by maxJobsHardCap=500" — but a long-lived deployment that
// adds and deletes thousands of cron jobs over weeks accumulates an
// unbounded sync.Map of dead *runInflight structs, never freed.
//
// The original ID-reuse split-CAS concern (a fresh AddJob colliding on
// the same 16-hex-char ID while the old execute() still holds the
// pointer to the OLD guard) is mitigated three ways at this entry
// point:
//
//  1. We only LoadAndDelete when running.Load() == false. If the gate
//     is held, leave the entry alone — the executeOpt goroutine still
//     holds the pointer and is about to releaseRun() on it. The caller
//     can re-attempt cleanup later (or accept the per-job-id leak;
//     bounded by jobs that get deleted while a run is in flight, an
//     even narrower window than the original maxJobsHardCap bound).
//  2. ID generation is crypto/rand 8 bytes (16 hex chars, 2^64 space);
//     for the maxJobsHardCap=500 working set the birthday-paradox
//     collision probability is ~2^-32 over the entire process lifetime
//     — far below any other production race we accept.
//  3. AddJob already retries on s.jobs collision (10 attempts, slog.Warn
//     each) so a re-used ID would not silently slip in undetected; the
//     window where new AddJob lands BEFORE the old run finishes is
//     vanishingly thin.
//
// Returns true if the entry was deleted. Safe to call after s.mu is
// released — sync.Map.LoadAndDelete needs no scheduler lock. Callers
// invoke this from postCleanup branches that already run lock-free.
func (s *Scheduler) cleanupRunningJobIfIdle(jobID string) bool {
	// R20260603140013-GO-2 (#1706): take the per-jobID gate around the whole
	// Load → running-check → CompareAndDelete sequence so it is atomic
	// relative to executeOpt's jobInflight-load→CAS pair (which holds the same
	// gate). This is what closes the previously-accepted double-execution
	// window: while we hold the gate, no executeOpt can be mid-load→CAS, so we
	// never delete an entry that a racing executeOpt is about to CAS-win on.
	gate := s.jobGateLock(jobID)
	gate.Lock()
	defer gate.Unlock()

	v, ok := s.runningJobs.Load(jobID)
	if !ok {
		return false
	}
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		// Defensive: an unexpected map value type implies the package
		// invariant was violated upstream.
		//
		// R040034-GO-7 (#1392): bump severity to slog.Error so the
		// invariant violation surfaces in journalctl. The previous
		// silent sweep meant a future regression that stored the wrong
		// type into runningJobs (a refactor returning a value-typed
		// snapshot, a stale closure, etc.) would be cleaned up without
		// any operator-visible signal until downstream code paths
		// observed the missing in-flight metadata.
		//
		// R260528-BUG-11: use CompareAndDelete on the observed v (not
		// LoadAndDelete on the jobID) so a concurrent jobInflight that
		// already replaced this stale entry with a fresh *runInflight
		// is not collateral damage. Mirrors the normal-path
		// CompareAndDelete below to keep both branches TOCTOU-safe
		// under the same single-flight contract.
		slog.Error("cron: runningJobs holds unexpected value type; sweeping",
			"job_id", jobID, "type", fmt.Sprintf("%T", v))
		s.runningJobs.CompareAndDelete(jobID, v)
		return true
	}
	if inf.running.Load() {
		// In-flight execute() goroutine still holds the pointer and is
		// about to releaseRun(); skip — leaking THIS one entry until the
		// next DeleteJob sweep is cheaper than risking a CAS-gate split
		// against a (vanishingly rare) ID-reuse collision.
		return false
	}
	// R20260527-GO-2 (#1270): use CompareAndDelete on the *runInflight
	// pointer rather than LoadAndDelete on the key. The Load+LoadAndDelete
	// pair is non-atomic — between the running.Load() check and the
	// LoadAndDelete, a concurrent executeOpt for an ID-reused jobID can
	// CompareAndSwap the gate to true and we'd then drop the now-active
	// entry. The next jobInflight call would LoadOrStore a fresh
	// *runInflight, leaving two goroutines holding distinct gate pointers
	// for the same jobID → double execution. CompareAndDelete only
	// succeeds when the map still holds OUR observed inf pointer; if a
	// fresh entry was stored it leaves the new one alone.
	//
	// R040034-CHANGES (#1416 review): CompareAndDelete-on-pointer (not
	// LoadAndDelete-on-key) closes the Load+delete TOCTOU for the case where
	// the map has already been swapped to a fresh *runInflight by a racing
	// AddJob+jobInflight — it only deletes when the map still holds OUR
	// observed inf pointer.
	//
	// R20260603140013-GO-2 (#1706): the adjacent window this comment used to
	// flag as a KNOWN remaining race — executeOpt having done
	//   inflight := s.jobInflight(j.ID)          // gets old *runInflight
	// just before this cleanup deletes the map entry, then CAS-winning on the
	// orphaned old gate while a second executeOpt LoadOrStores a fresh gate →
	// double execution — is now closed. This whole function runs under the
	// per-jobID gate (see top of function), and executeOpt holds the same gate
	// across its jobInflight-load→CAS pair, so the two sequences are mutually
	// exclusive: cleanup can only observe the gate as idle (no executeOpt is
	// mid-load→CAS) or already-running (CAS won → running.Load()==true above →
	// we returned without deleting). The orphan-in-between state is no longer
	// reachable, so the fix no longer rests on the crypto/rand ID-reuse
	// improbability argument.
	s.runningJobs.CompareAndDelete(jobID, inf)
	return true
}

// jobInflight returns a lazily created *runInflight per job ID. The
// embedded atomic.Bool keeps the original CAS-gate semantics (used by
// executeOpt to reject concurrent runs); the surrounding metadata fields
// expose RunID/StartedAt/Phase to the list API for the cron-run-history
// P0 visibility work.
//
// Entries are reclaimed on DeleteJob via cleanupRunningJobIfIdle when
// the CAS gate is idle (R242-ARCH-15 / #758). The prior never-cleanup
// policy was a worst-case bound of maxJobsHardCap=500 entries; in long-
// lived deployments that delete and re-add jobs the working set could
// grow without limit.
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
//
// R249-CR-4 / R260528-ARCH-7 (#948 / #1368): containsSessionID and
// buildKnownSessionsSet both open-coded the s.runningJobs.Range +
// *runInflight type-assert + snapshot + running/non-empty guard. Folding the
// boilerplate here decouples both callers from the s.runningJobs sync.Map
// representation (one of the fields the god-struct issue flags) and keeps the
// inflight-view contract in a single place.
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
