package cron

import (
	"errors"
	"testing"
)

// TestResumeJob_RollbackOnPersistFailure pins R20260531070014-CR-2:
// resumeJobLocked → registerJob mutates j.entryID + j.cachedPeriod +
// j.cachedSched and flips j.Paused=false BEFORE persistJobsLocked runs.
// A persist failure after op-success would leave in-memory Paused=false with
// a live cron entry while disk still shows Paused=true — on restart the
// scheduler re-registers the schedule on top of the surviving entry, producing
// a double-fire. After the rollback fix, in-memory state matches disk.
func TestResumeJob_RollbackOnPersistFailure(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed is Paused=true with no cron entry.
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
		t.Fatalf("seed precondition violated: entryID=%v want 0 (paused job)", preEntryID)
	}

	withFailingMarshal(t, s)

	_, err := s.ResumeJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJob err = %v, want ErrPersistFailed", err)
	}

	// In-memory state must be rolled back: still paused, entryID cleared,
	// cachedPeriod restored.
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back ResumeJob", id)
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

// TestResumeJob_RollbackRemovesCronEntry pins the lock-order safety of
// R20260531070014-CR-2: the rollback closure captures the freshly-registered
// entryID and the actual s.cron.Remove is deferred until AFTER
// withJobByPrefix returns (s.mu released). This test verifies that after
// a rolled-back ResumeJob the orphaned cron entry is gone and NextRun returns
// zero — the scheduler is not left with a live entry for a still-paused job.
func TestResumeJob_RollbackRemovesCronEntry(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed is Paused=true with no cron entry.
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

	_, err := s.ResumeJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJob err = %v, want ErrPersistFailed", err)
	}

	// After rollback the in-memory entryID must be cleared back to 0 and
	// NextRun must return zero (no live cron entry for this job).
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back ResumeJob", id)
	}
	if got.entryID != 0 {
		t.Fatalf("rollback failed: entryID=%v want 0", got.entryID)
	}
	if !got.Paused {
		t.Fatalf("rollback failed: Paused=false; want true (still paused on disk)")
	}
	if nr := s.NextRun(got); !nr.IsZero() {
		t.Errorf("NextRun after rolled-back ResumeJob = %v; want zero (orphaned entry removed)", nr)
	}
}

// TestResumeJob_RollbackRestoresCachedSched verifies that the CR-2 rollback
// also restores j.cachedSched to its pre-op value. HasMissedScheduleCached
// uses j.cachedSched for the 1Hz dashboard fanout; a stale (post-resume)
// schedule on a still-paused job would cause missed-schedule false-positives.
func TestResumeJob_RollbackRestoresCachedSched(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed is Paused=true; paused jobs have cachedSched=nil.
	s.mu.RLock()
	j := s.jobs[id]
	if j == nil {
		s.mu.RUnlock()
		t.Fatalf("job %q missing", id)
	}
	preSched := j.cachedSched // nil for a freshly-seeded paused job
	s.mu.RUnlock()

	withFailingMarshal(t, s)

	_, err := s.ResumeJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJob err = %v, want ErrPersistFailed", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back ResumeJob", id)
	}
	if got.cachedSched != preSched {
		t.Fatalf("rollback failed: cachedSched=%v want %v (pre-op value not restored)",
			got.cachedSched, preSched)
	}
}
