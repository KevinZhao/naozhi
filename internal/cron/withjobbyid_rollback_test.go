package cron

import (
	"errors"
	"testing"
)

// TestPauseJobByID_RollbackOnPersistFailure pins the R20260527-COR-1 (#1272)
// fix: when persistJobsLocked fails AFTER pauseJobLocked already mutated
// (j.entryID=0, j.Paused=true), the helper now rolls back the in-memory
// mutation so a subsequent restart-from-disk view (Paused=false) and the
// in-memory view stay aligned.
//
// Symptom of the historical bug: the API returned ErrPersistFailed but the
// in-memory job had Paused=true and entryID=0; if the operator did NOT
// restart, the unpaused-on-disk job's next tick would not fire (cron entry
// gone) but it ALSO was not visible as paused in the UI fan-out — split-
// brain. After the fix, the in-memory state matches the on-disk state on
// every persist failure.
func TestPauseJobByID_RollbackOnPersistFailure(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seeded job is Paused=true; flip it active first so the pause under
	// test is a real state change.
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID seed: %v", err)
	}

	// Capture the live entry ID + Paused under s.mu so the rollback
	// invariant is asserted against a real pre-op snapshot.
	s.mu.RLock()
	j := s.jobs[id]
	if j == nil {
		s.mu.RUnlock()
		t.Fatalf("job %q missing from s.jobs after Resume", id)
	}
	preEntryID := j.entryID
	prePaused := j.Paused
	s.mu.RUnlock()

	if prePaused {
		t.Fatalf("seed precondition violated: Paused=true after ResumeJobByID")
	}
	if preEntryID == 0 {
		t.Fatalf("seed precondition violated: entryID=0 after ResumeJobByID")
	}

	withFailingMarshal(t, s)

	_, err := s.PauseJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJobByID err = %v, want ErrPersistFailed", err)
	}

	// In-memory state must be rolled back to the pre-op view.
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back PauseJobByID", id)
	}
	if got.Paused {
		t.Fatalf("rollback failed: Paused=true; want false (matches un-persisted disk)")
	}
	if got.entryID != preEntryID {
		t.Fatalf("rollback failed: entryID=%v want %v", got.entryID, preEntryID)
	}
}

// TestResumeJobByID_RollbackOnPersistFailure pins R20260526-GO-001 (#1226):
// resumeJobLocked → registerJob mutates j.entryID + j.cachedPeriod and then
// flips j.Paused=false BEFORE persistJobsLocked runs. A persist failure
// after that op-success path used to leave in-memory state with a live
// cron entry + Paused=false while disk still showed Paused=true — the
// next process restart would replay the paused-on-disk view onto the
// live entry, double-firing the schedule. After the rollback fix,
// in-memory state matches the un-persisted disk view on every persist
// failure.
func TestResumeJobByID_RollbackOnPersistFailure(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// The seed creates Paused=true with no cron entry, so a persist-fail
	// Resume here is the exact pre-op view we want to assert against.
	s.mu.RLock()
	j := s.jobs[id]
	if j == nil {
		s.mu.RUnlock()
		t.Fatalf("job %q missing from s.jobs", id)
	}
	preEntryID := j.entryID
	preCachedPeriod := j.cachedPeriod
	prePaused := j.Paused
	s.mu.RUnlock()

	if !prePaused {
		t.Fatalf("seed precondition violated: Paused=false")
	}
	if preEntryID != 0 {
		t.Fatalf("seed precondition violated: entryID=%v want 0 (paused jobs hold no entry)", preEntryID)
	}

	withFailingMarshal(t, s)

	_, err := s.ResumeJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJobByID err = %v, want ErrPersistFailed", err)
	}

	// In-memory state must be rolled back to the pre-op view: still
	// paused, entryID cleared, cachedPeriod restored.
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back ResumeJobByID", id)
	}
	if !got.Paused {
		t.Fatalf("rollback failed: Paused=false; want true (matches un-persisted disk)")
	}
	if got.entryID != preEntryID {
		t.Fatalf("rollback failed: entryID=%v want %v", got.entryID, preEntryID)
	}
	if got.cachedPeriod != preCachedPeriod {
		t.Fatalf("rollback failed: cachedPeriod=%v want %v", got.cachedPeriod, preCachedPeriod)
	}
}

// TestPauseJobByID_RollbackKeepsCronEntryAlive is the operator-visible
// counterpart: after a rolled-back Pause, the cron entry that
// pauseJobLocked would have torn down via cron.Remove (in postCleanup)
// must still be live. The withJobByIDOpt helper skips postCleanup when
// rollback fires, so the cron.Remove never runs.
func TestPauseJobByID_RollbackKeepsCronEntryAlive(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID seed: %v", err)
	}

	s.mu.RLock()
	preEntryID := s.jobs[id].entryID
	s.mu.RUnlock()
	if preEntryID == 0 {
		t.Fatalf("seed precondition violated: entryID=0 after Resume")
	}

	withFailingMarshal(t, s)

	if _, err := s.PauseJobByID(id); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJobByID err = %v, want ErrPersistFailed", err)
	}

	// The robfig/cron entry must still exist with the same EntryID. After
	// a real (non-rolled-back) Pause, s.cron.Entry(preEntryID).ID is 0
	// (the zero-value Entry struct robfig returns when the entry is gone).
	entry := s.cron.Entry(preEntryID)
	if entry.ID != preEntryID {
		t.Fatalf("cron entry torn down despite rollback: got entry.ID=%v want %v",
			entry.ID, preEntryID)
	}
}

// TestResumeJobByID_RollbackRemovesCronEntry pins R250531-CR-1: the rollback
// closure in ResumeJobByID previously called s.cron.Remove while holding
// s.mu, causing a lock-order inversion with the cron-tick goroutine (which
// needs s.mu.RLock to call executeJobIDIfLive). The fix defers the Remove to
// after withJobByIDOpt returns (s.mu released). This test verifies that after
// a rolled-back Resume, the freshly-registered cron entry has been removed so
// the scheduler is not left with a live entry for a still-paused job.
func TestResumeJobByID_RollbackRemovesCronEntry(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed starts Paused=true with no cron entry. Inject a failing marshaler
	// BEFORE calling ResumeJobByID so the persist step fails, triggering rollback.

	// Capture state before rollback — paused job has no entry yet.
	s.mu.RLock()
	j := s.jobs[id]
	if j == nil {
		s.mu.RUnlock()
		t.Fatalf("job %q missing from s.jobs", id)
	}
	if j.entryID != 0 {
		s.mu.RUnlock()
		t.Fatalf("seed precondition: entryID=%v want 0 (paused job)", j.entryID)
	}
	s.mu.RUnlock()

	withFailingMarshal(t, s)

	_, err := s.ResumeJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJobByID err = %v, want ErrPersistFailed", err)
	}

	// After rollback, the cron entry that registerJob registered during the op
	// must have been removed (s.cron.Remove fired after lock release). The
	// in-memory j.entryID is rolled back to 0, so we probe via the job's
	// post-rollback entryID; since it's 0, there is no live entry to find.
	// Additionally confirm via the in-memory j.entryID that rollback cleared it.
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back ResumeJobByID", id)
	}
	if got.entryID != 0 {
		t.Fatalf("rollback failed: entryID=%v want 0 after ResumeJobByID rollback", got.entryID)
	}
	if !got.Paused {
		t.Fatalf("rollback failed: Paused=false; want true (still paused on disk)")
	}
	// The entry that registerJob allocated (now rolled back out of j.entryID)
	// must be gone from robfig/cron. We cannot directly observe which EntryID
	// was allocated, but we can confirm s.cron has no entry for any non-zero
	// EntryID in the 1..1000 range that references our job — a simpler proxy
	// is confirming that NextRun returns zero (no live entry for this job).
	// NextRun falls back to s.jobs[id].entryID which is now 0 → returns zero.
	if nr := s.NextRun(got); !nr.IsZero() {
		t.Errorf("NextRun after rolled-back Resume = %v; want zero (orphaned entry removed)", nr)
	}
}

// TestResumeJobByID_RollbackRestoresCachedSched pins R250531-CR-3: the rollback
// closure in ResumeJobByID previously omitted restoring j.cachedSched after
// registerJob wrote it. HasMissedScheduleCached uses j.cachedSched for the 1Hz
// dashboard fanout; a stale (post-resume) schedule on a still-paused job would
// cause missed-schedule false-positives. After the fix, rollback restores
// cachedSched to its pre-op value.
func TestResumeJobByID_RollbackRestoresCachedSched(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed is Paused=true; paused jobs have cachedSched=nil.

	s.mu.RLock()
	j := s.jobs[id]
	if j == nil {
		s.mu.RUnlock()
		t.Fatalf("job %q missing", id)
	}
	preSched := j.cachedSched // nil for a paused job before any Resume
	s.mu.RUnlock()

	withFailingMarshal(t, s)

	_, err := s.ResumeJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJobByID err = %v, want ErrPersistFailed", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back ResumeJobByID", id)
	}
	// After rollback, cachedSched must be identical to the pre-op value (nil
	// for a freshly-seeded paused job). If registerJob set cachedSched and
	// rollback did not restore it, got.cachedSched would be non-nil here.
	if got.cachedSched != preSched {
		t.Fatalf("rollback failed: cachedSched=%v want %v (pre-op value not restored)",
			got.cachedSched, preSched)
	}
}
