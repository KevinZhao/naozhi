package cron

import (
	"errors"
	"testing"
)

// TestPauseJob_RollbackOnPersistFailure pins R20260531070014-CR-1:
// when persistJobsLocked fails AFTER pauseJobLocked already mutated
// (j.entryID=0, j.Paused=true), PauseJob must roll back the in-memory
// mutation so disk (Paused=false, un-persisted) and memory stay aligned.
// Without the fix, a process restart sees Paused=false on disk and
// re-registers the schedule, while the in-memory view sees Paused=true
// with no cron entry — the "ghost-paused job that never fires" split-brain.
func TestPauseJob_RollbackOnPersistFailure(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed is Paused=true; flip to active first so PauseJob is a real change.
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID seed: %v", err)
	}

	j := s.jobForTest(t, id)
	preEntryID := j.entryID
	prePaused := j.Paused

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
	got := s.jobForTest(t, id)
	if got.Paused {
		t.Fatalf("rollback failed: Paused=true; want false (matches un-persisted disk)")
	}
	if got.entryID != preEntryID {
		t.Fatalf("rollback failed: entryID=%v want %v", got.entryID, preEntryID)
	}
}

// TestPauseJob_RollbackKeepsCronEntryAlive verifies the operator-visible
// consequence of the CR-1 rollback: after a rolled-back PauseJob, the
// robfig/cron entry the pause would have retired must still be live. A
// rolled-back pause hands finishMutation no entry to remove, so
// cron.Remove never runs and the next tick still fires the active job.
func TestPauseJob_RollbackKeepsCronEntryAlive(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID seed: %v", err)
	}

	preEntryID := s.jobForTest(t, id).entryID
	if preEntryID == 0 {
		t.Fatalf("seed precondition violated: entryID=0 after Resume")
	}

	withFailingMarshal(t, s)

	if _, err := s.PauseJob(id[:4], "feishu", "chat1"); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJob err = %v, want ErrPersistFailed", err)
	}

	// The robfig/cron entry must still exist with the same EntryID.
	// After a real (non-rolled-back) Pause, s.cron.Entry(preEntryID).ID is 0.
	entry := s.cron.Entry(preEntryID)
	if entry.ID != preEntryID {
		t.Fatalf("cron entry torn down despite rollback: got entry.ID=%v want %v",
			entry.ID, preEntryID)
	}
}
