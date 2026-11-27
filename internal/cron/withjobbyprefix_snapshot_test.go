package cron

import (
	"path/filepath"
	"testing"
)

// TestWithJobByPrefix_ReturnsSnapshot pins R250531-CR-2: mutateByPrefix must
// return a value-copy snapshot of the Job (like mutateByID's "snapshot =
// *j" pattern from R242-GO-3/#548) rather than the live *Job pointer from
// s.tbl.jobs. Without the copy, a concurrent UpdateJob or SetJobPrompt can race
// on the string fields of the returned *Job while the caller reads them.
//
// This test verifies the structural invariant: the address of the returned Job
// must not equal the address of the live entry in s.tbl.jobs, proving the caller
// holds a stable copy, not a shared pointer.
func TestWithJobByPrefix_ReturnsSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	j := &Job{
		Schedule: "@every 1h",
		Prompt:   "original-prompt",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true,
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// PauseJob is a no-op here since the job is already paused, but the real
	// ResumeJob path exercises mutateByPrefix with a live op; let's use
	// ResumeJob to exercise the full path (op mutates then snapshots).
	got, err := s.ResumeJob(j.ID[:4], "feishu", "chat1")
	if err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if got == nil {
		t.Fatalf("ResumeJob returned nil *Job")
	}

	// Key invariant: caller gets a copy, not the live pointer.
	s.tblForTest().mu.RLock()
	live := s.tblForTest().jobs[j.ID]
	s.tblForTest().mu.RUnlock()

	if live == nil {
		t.Fatalf("live job gone from s.tbl.jobs after ResumeJob")
	}
	if got == live {
		t.Errorf("mutateByPrefix returned live *Job pointer; want a stable snapshot copy")
	}
	// Values must match at this point (no concurrent mutation between Resume
	// and this check).
	if got.ID != live.ID {
		t.Errorf("snapshot ID=%q != live ID=%q", got.ID, live.ID)
	}
}
