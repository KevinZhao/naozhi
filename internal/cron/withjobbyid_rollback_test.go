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
