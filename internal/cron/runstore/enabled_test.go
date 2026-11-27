package runstore

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"
)

// TestStore_NilBehavesDisabled: a nil *Store is how a Scheduler built without
// NewScheduler holds its history, and cron calls the store directly, so every
// method must treat nil as "persistence off" instead of panicking.
func TestStore_NilBehavesDisabled(t *testing.T) {
	t.Parallel()
	var s *Store
	if s.Enabled() {
		t.Fatal("nil *Store must report Enabled()==false")
	}
	if df, ot := s.WriteFailedTotals(); df != 0 || ot != 0 {
		t.Errorf("WriteFailedTotals = (%d,%d), want (0,0)", df, ot)
	}
	jobID, runID := mustGenerateID(), mustGenerateRunID()
	s.Append(&CronRun{JobID: jobID, RunID: runID})
	s.DeleteJob(jobID)
	s.DropOrphanRun(jobID, runID)
	s.TrimAll(context.Background(), time.Now())
	if got := s.List(jobID, 10, time.Time{}); got != nil {
		t.Errorf("List = %v, want nil", got)
	}
	if got := s.Recent(jobID, 5); got != nil {
		t.Errorf("Recent = %v, want nil", got)
	}
	if got := s.RecentSessionIDs(jobID, 5); got != nil {
		t.Errorf("RecentSessionIDs = %v, want nil", got)
	}
	if run, err := s.Get(jobID, runID); run != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Get = (%v, %v), want (nil, fs.ErrNotExist)", run, err)
	}
	if s.Dir() != "" || s.KeepCount() != 0 || s.KeepWindow() != 0 {
		t.Errorf("Dir/KeepCount/KeepWindow = %q/%d/%v, want zero values", s.Dir(), s.KeepCount(), s.KeepWindow())
	}
}
