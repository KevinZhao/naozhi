package runstore

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runlog"
)

// TestStoreEnabled_FoldsNilAndDisabled pins R249-ARCH-29 (#993): the
// Enabled() predicate must collapse the two historically-separate "off"
// signals — a nil *Store receiver and the disabled flag — into one
// gate so external callers stop mixing a nil check with the
// method-internal Enabled() guard (now the shared layout's predicate).
func TestStoreEnabled_FoldsNilAndDisabled(t *testing.T) {
	t.Parallel()

	var nilStore *Store
	if nilStore.Enabled() {
		t.Fatal("nil *Store must report Enabled()==false")
	}

	disabled := &Store{layout: runlog.New(runlog.Options{})}
	if disabled.Enabled() {
		t.Fatal("disabled Store must report Enabled()==false")
	}

	live := &Store{layout: runlog.New(runlog.Options{Root: t.TempDir(), Label: "cron run"})}
	if !live.Enabled() {
		t.Fatal("non-nil, non-disabled Store must report Enabled()==true")
	}
}

// TestStore_NilBehavesDisabled: a nil *Store is how a Scheduler built without
// NewScheduler holds its history, and cron calls the store directly, so every
// method must treat nil as "persistence off" instead of panicking.
func TestStore_NilBehavesDisabled(t *testing.T) {
	t.Parallel()
	var s *Store
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
	if h := s.Health(); h != (Health{}) {
		t.Errorf("Health = %+v, want zero", h)
	}
	if s.Dir() != "" || s.KeepCount() != 0 || s.KeepWindow() != 0 {
		t.Errorf("Dir/KeepCount/KeepWindow = %q/%d/%v, want zero values", s.Dir(), s.KeepCount(), s.KeepWindow())
	}
}
