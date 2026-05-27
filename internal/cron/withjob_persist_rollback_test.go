package cron

// R20260527-COR-1 (#1272) regression tests: when withJobByID's op succeeds
// but persistJobsLocked fails, the in-memory mutation must roll back so
// disk and memory agree. Without rollback, PauseJobByID would leave
// j.Paused=true / j.entryID=0 in memory while disk still showed
// Paused=false; a restart replayed the un-paused state, "resurrecting"
// the job from the dashboard's perspective.

import (
	"errors"
	"testing"
)

// TestWithJobByID_PauseRollbackOnPersistFail asserts that a failed
// persistJobsLocked during PauseJobByID rolls back the in-memory
// mutation so the next read sees the pre-mutation state.
func TestWithJobByID_PauseRollbackOnPersistFail(t *testing.T) {
	t.Parallel()
	s, jobID := newTestSchedulerForPersist(t)

	// Resume the seed job so we have a non-paused starting state
	// to mutate. (The seed is created Paused=true to skip cron
	// registration.) After this, j.Paused=false + j.entryID != 0.
	if _, err := s.ResumeJobByID(jobID); err != nil {
		t.Fatalf("ResumeJobByID seed setup: %v", err)
	}

	// Capture pre-mutation state under lock for comparison.
	s.mu.RLock()
	preJob := s.jobs[jobID]
	prePaused := preJob.Paused
	preEntry := preJob.entryID
	s.mu.RUnlock()
	if prePaused {
		t.Fatalf("seed job should be active before pause; got Paused=true")
	}
	if preEntry == 0 {
		t.Fatalf("seed job should have non-zero entryID before pause")
	}

	// Inject a failing marshaler so persistJobsLocked errors.
	withFailingMarshal(t, s)

	// PauseJobByID must surface ErrPersistFailed AND restore the
	// in-memory state (j.Paused=false, j.entryID=preEntry).
	got, err := s.PauseJobByID(jobID)
	if err == nil {
		t.Fatalf("PauseJobByID: expected ErrPersistFailed, got nil")
	}
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJobByID err = %v; want ErrPersistFailed", err)
	}
	if got != nil {
		t.Errorf("PauseJobByID returned non-nil Job on persist failure: %+v", got)
	}

	// In-memory state must match pre-mutation.
	s.mu.RLock()
	postJob := s.jobs[jobID]
	postPaused := postJob.Paused
	postEntry := postJob.entryID
	s.mu.RUnlock()
	if postPaused {
		t.Errorf("rollback failed: j.Paused = true after persist failure; want false (R20260527-COR-1 / #1272)")
	}
	if postEntry != preEntry {
		t.Errorf("rollback failed: j.entryID = %d after persist failure; want %d (cron entry should still be registered)",
			postEntry, preEntry)
	}
}

// TestWithJobByID_ResumeRollbackOnPersistFail asserts the same contract
// for ResumeJobByID — a persist failure must un-resume (re-pause +
// drop the cron entry) so disk and memory agree.
func TestWithJobByID_ResumeRollbackOnPersistFail(t *testing.T) {
	t.Parallel()
	s, jobID := newTestSchedulerForPersist(t)

	// Seed is already Paused=true. Capture entry state.
	s.mu.RLock()
	preJob := s.jobs[jobID]
	prePaused := preJob.Paused
	preEntry := preJob.entryID
	s.mu.RUnlock()
	if !prePaused {
		t.Fatalf("seed job should be paused before resume; got Paused=false")
	}
	if preEntry != 0 {
		t.Fatalf("seed paused job should have zero entryID; got %d", preEntry)
	}

	withFailingMarshal(t, s)

	got, err := s.ResumeJobByID(jobID)
	if err == nil {
		t.Fatalf("ResumeJobByID: expected ErrPersistFailed, got nil")
	}
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJobByID err = %v; want ErrPersistFailed", err)
	}
	if got != nil {
		t.Errorf("ResumeJobByID returned non-nil Job on persist failure: %+v", got)
	}

	// In-memory state must match pre-mutation: still paused, entryID=0.
	s.mu.RLock()
	postJob := s.jobs[jobID]
	postPaused := postJob.Paused
	postEntry := postJob.entryID
	s.mu.RUnlock()
	if !postPaused {
		t.Errorf("rollback failed: j.Paused = false after persist failure; want true (R20260527-COR-1 / #1272)")
	}
	if postEntry != 0 {
		t.Errorf("rollback failed: j.entryID = %d after persist failure; want 0", postEntry)
	}
}
