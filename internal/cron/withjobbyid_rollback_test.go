package cron

import (
	"errors"
	"testing"
)

// TestPauseJobByID_RollbackOnPersistFailure pins R20260527-COR-1 (#1272):
// when the persist after a pause fails, memory goes back to the un-persisted
// disk view — still active, same entryID — and the cron entry the pause would
// have torn down is still live. Before, the API returned ErrPersistFailed while
// memory held Paused=true / entryID=0: an active-on-disk job that never fired
// and was not shown as paused either.
func TestPauseJobByID_RollbackOnPersistFailure(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// The seed is paused; resume it so the pause under test is a real change.
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID seed: %v", err)
	}
	preEntryID := s.jobForTest(t, id).entryID
	if preEntryID == 0 {
		t.Fatalf("seed precondition violated: entryID=0 after ResumeJobByID")
	}

	withFailingMarshal(t, s)
	if _, err := s.PauseJobByID(id); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJobByID err = %v, want ErrPersistFailed", err)
	}

	got := s.jobForTest(t, id)
	if got.Paused || got.entryID != preEntryID {
		t.Fatalf("rollback failed: Paused=%v entryID=%v, want false and %v", got.Paused, got.entryID, preEntryID)
	}
	if entry := s.cron.Entry(preEntryID); entry.ID != preEntryID {
		t.Fatalf("cron entry torn down despite rollback: got entry.ID=%v want %v", entry.ID, preEntryID)
	}
}

// TestResumeJobByID_RollbackOnPersistFailure pins R20260526-GO-001 (#1226),
// R250531-CR-1 and R250531-CR-3: resume flips Paused=false before the persist,
// so a persist failure must leave the job exactly as the paused-on-disk view —
// still paused, no entryID, no live cron entry (NextRun zero), cachedPeriod and
// cachedSched at their pre-op values — or a restart would replay the paused
// view onto live state and double-fire the schedule.
func TestResumeJobByID_RollbackOnPersistFailure(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	pre := s.jobForTest(t, id)
	if !pre.Paused || pre.entryID != 0 {
		t.Fatalf("seed precondition violated: Paused=%v entryID=%v, want paused with no entry", pre.Paused, pre.entryID)
	}

	withFailingMarshal(t, s)
	if _, err := s.ResumeJobByID(id); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJobByID err = %v, want ErrPersistFailed", err)
	}

	got := s.jobForTest(t, id)
	if !got.Paused {
		t.Fatalf("rollback failed: Paused=false; want true (matches un-persisted disk)")
	}
	if got.entryID != 0 {
		t.Fatalf("rollback failed: entryID=%v want 0", got.entryID)
	}
	if got.cachedPeriod != pre.cachedPeriod {
		t.Fatalf("rollback failed: cachedPeriod=%v want %v", got.cachedPeriod, pre.cachedPeriod)
	}
	if got.cachedSched != pre.cachedSched {
		t.Fatalf("rollback failed: cachedSched=%v want %v", got.cachedSched, pre.cachedSched)
	}
	if nr := s.NextRun(&got); !nr.IsZero() {
		t.Errorf("NextRun after rolled-back Resume = %v; want zero (no live entry)", nr)
	}
}
