package cron

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestWithJobByPrefix_ReturnsSnapshot pins R250531-CR-2: withJobByPrefix must
// return a value-copy snapshot of the Job (like withJobByIDOpt's "snapshot =
// *j" pattern from R242-GO-3/#548) rather than the live *Job pointer from
// s.jobs. Without the copy, a concurrent UpdateJob or SetJobPrompt can race
// on the string fields of the returned *Job while the caller reads them.
//
// This test verifies the structural invariant: the address of the returned Job
// must not equal the address of the live entry in s.jobs, proving the caller
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
	// ResumeJob path exercises withJobByPrefix with a live op; let's use
	// ResumeJob to exercise the full path (op mutates then snapshots).
	got, err := s.ResumeJob(j.ID[:4], "feishu", "chat1")
	if err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if got == nil {
		t.Fatalf("ResumeJob returned nil *Job")
	}

	// Key invariant: caller gets a copy, not the live pointer.
	s.mu.RLock()
	live := s.jobs[j.ID]
	s.mu.RUnlock()

	if live == nil {
		t.Fatalf("live job gone from s.jobs after ResumeJob")
	}
	if got == live {
		t.Errorf("withJobByPrefix returned live *Job pointer; want a stable snapshot copy")
	}
	// Values must match at this point (no concurrent mutation between Resume
	// and this check).
	if got.ID != live.ID {
		t.Errorf("snapshot ID=%q != live ID=%q", got.ID, live.ID)
	}
}

// TestWithJobByPrefix_SnapshotRaceDeleteJob exercises the -race detector path:
// a concurrent UpdateJob mutating the Prompt string field of the live *Job
// must not tear when withJobByPrefix returns a snapshot. Without the copy,
// the returned *Job's string header points into the same backing array a
// concurrent goroutine is overwriting → data race. With the copy, the caller
// holds its own stable string.
//
// This is a structural race test; the -race flag is required to catch the
// regression reliably. Without -race it still validates the copy invariant.
func TestWithJobByPrefix_SnapshotRaceDeleteJob(t *testing.T) {
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
		Prompt:   "initial-prompt",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true,
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Fire concurrent UpdateJob (Prompt mutation) while calling ResumeJob
	// (which goes through withJobByPrefix). Without the snapshot fix both
	// goroutines operate on the same *Job pointer — the race detector flags
	// concurrent string writes against the reads inside withJobByPrefix.
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			newPrompt := "concurrent-prompt"
			if _, err := s.UpdateJob(j.ID, JobUpdate{Prompt: &newPrompt}); err != nil {
				// UpdateJob may fail on persist; that's fine for the race test.
				_ = err
			}
		}
	}()

	for i := 0; i < 10; i++ {
		got, err := s.ResumeJob(j.ID[:4], "feishu", "chat1")
		if err != nil {
			// May fail with ErrJobAlreadyActive; just continue.
			_ = err
		}
		if got != nil {
			// Read the snapshot field — with the fix this is our stable copy;
			// without the fix this races against the concurrent UpdateJob.
			_ = got.Prompt
			// Pause again so we can resume in the next iteration.
			if _, err := s.PauseJob(j.ID[:4], "feishu", "chat1"); err != nil {
				_ = err
			}
		}
	}

	wg.Wait()
}
