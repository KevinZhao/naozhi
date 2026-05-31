package cron

import (
	"errors"
	"testing"
)

// TestPauseJob_RollbackOnPersistFailure pins R20260531A-BUG-4: when
// persistJobsLocked fails AFTER pauseJobLocked already mutated
// (j.Paused=true, j.entryID=0), PauseJob (IM-prefix path) now rolls back
// the in-memory mutation so on-disk state (Paused=false) and in-memory
// state stay aligned — preventing a double-fire on restart.
func TestPauseJob_RollbackOnPersistFailure(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed is Paused=true; flip it active first so the pause under test is a
	// real state change.
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID seed: %v", err)
	}

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

	_, err := s.PauseJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJob err = %v, want ErrPersistFailed", err)
	}

	// In-memory state must be rolled back to the pre-op view.
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back PauseJob", id)
	}
	if got.Paused {
		t.Fatalf("rollback failed: Paused=true; want false (matches un-persisted disk)")
	}
	if got.entryID != preEntryID {
		t.Fatalf("rollback failed: entryID=%v want %v", got.entryID, preEntryID)
	}
}

// TestPauseJob_RollbackKeepsCronEntryAlive pins R20260531A-BUG-4: after a
// rolled-back PauseJob (IM-prefix), the cron entry that pauseJobLocked would
// have removed via postCleanup must still be alive (postCleanup is skipped
// on rollback).
func TestPauseJob_RollbackKeepsCronEntryAlive(t *testing.T) {
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

	if _, err := s.PauseJob(id[:4], "feishu", "chat1"); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJob err = %v, want ErrPersistFailed", err)
	}

	// The robfig/cron entry must still exist with the same EntryID.
	entry := s.cron.Entry(preEntryID)
	if entry.ID != preEntryID {
		t.Fatalf("cron entry torn down despite rollback: got entry.ID=%v want %v",
			entry.ID, preEntryID)
	}
}

// TestResumeJob_RollbackOnPersistFailure pins R20260531A-GO-002: when
// persistJobsLocked fails after resumeJobLocked → registerJob mutated
// (j.entryID/cachedPeriod/cachedSched/Paused), ResumeJob (IM-prefix path)
// now rolls back the in-memory state so a restart does not double-fire.
func TestResumeJob_RollbackOnPersistFailure(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed starts Paused=true with no cron entry.
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

	// In-memory state must be rolled back to the pre-op view.
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

// TestResumeJob_RollbackRemovesCronEntry pins R20260531A-GO-002: after a
// rolled-back ResumeJob (IM-prefix), the freshly-registered cron entry must
// have been removed (s.cron.Remove fires after withJobByPrefix returns, i.e.
// after s.mu is released — mirrors ResumeJobByID's CR-1 hoist).
func TestResumeJob_RollbackRemovesCronEntry(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

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

	// After rollback, entryID is restored to 0 and NextRun returns zero.
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[id]
	if got == nil {
		t.Fatalf("job %q vanished after rolled-back ResumeJob", id)
	}
	if got.entryID != 0 {
		t.Fatalf("rollback failed: entryID=%v want 0 after ResumeJob rollback", got.entryID)
	}
	if !got.Paused {
		t.Fatalf("rollback failed: Paused=false; want true (still paused on disk)")
	}
	if nr := s.NextRun(got); !nr.IsZero() {
		t.Errorf("NextRun after rolled-back ResumeJob = %v; want zero (orphaned entry removed)", nr)
	}
}

// TestResumeJob_RollbackRestoresCachedSched pins R20260531A-GO-002: the
// rollback must restore j.cachedSched to its pre-op value so
// HasMissedScheduleCached does not produce false-positives for a still-paused
// job. Mirrors TestResumeJobByID_RollbackRestoresCachedSched.
func TestResumeJob_RollbackRestoresCachedSched(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

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
