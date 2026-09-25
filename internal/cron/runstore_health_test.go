package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunStoreHealth_ReflectsARealWriteFailure: a record that cannot be
// written shows up in the snapshot /health serves. The counter already existed
// (#1338); what was missing is the path from it to anyone who could see it.
func TestRunStoreHealth_ReflectsARealWriteFailure(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: &fakeRouter{}})
	if h := s.RunStoreHealth(); !h.Enabled || h.WriteFailedOther != 0 {
		t.Fatalf("fresh store = %+v, want enabled with zero counters", h)
	}

	jobID := mustGenerateID()
	// Prime the job dir, then make it unwritable so the record write fails.
	if _, err := s.runStore.layout.EnsureOwnerDir(jobID); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.runStore.rootDir(), jobID)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	s.runStore.Append(makeRun(jobID, time.Now()))

	if ents, _ := os.ReadDir(dir); len(ents) > 0 {
		t.Skip("write into a 0500 dir succeeded (running as root?); failure path not exercised")
	}
	if h := s.RunStoreHealth(); h.WriteFailedOther != 1 {
		t.Errorf("WriteFailedOther = %d, want 1 — the lost record never reached the health snapshot", h.WriteFailedOther)
	}
}

// TestRunStoreHealth_DisabledStoreReportsDisabled: a scheduler with no store
// must say so, so /health omits the section instead of serving zeros that
// look like a healthy store.
func TestRunStoreHealth_DisabledStoreReportsDisabled(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})
	if h := s.RunStoreHealth(); h.Enabled {
		t.Errorf("no-persistence scheduler reported %+v, want Enabled=false", h)
	}
	var nilSched *Scheduler
	if h := nilSched.RunStoreHealth(); h.Enabled {
		t.Error("nil scheduler must report disabled")
	}
}
