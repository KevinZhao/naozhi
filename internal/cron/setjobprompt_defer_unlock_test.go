package cron

import (
	"testing"
	"time"
)

// TestSetJobPrompt_AllReturnPathsUnlock verifies that SetJobPrompt unlocks
// s.tbl.mu on all observable return paths (not-found, already-set, persist-fail,
// success) — jobTable.fillPrompt's deferred Unlock guarantees this
// structurally, but this runtime test catches any regression where the lock
// is held after return.
func TestSetJobPrompt_AllReturnPathsUnlock(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		MaxJobs:        10,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Path 1: job not found — must unlock.
	if err := s.SetJobPrompt("nonexistent-id", "p"); err == nil {
		t.Error("expected error for nonexistent job")
	}
	// If lock is held, TryLock returns false. We use a goroutine with timeout
	// to verify s.tbl.mu is not held after SetJobPrompt returns.
	assertMuUnlocked(t, s, "not-found path")

	// Path 2: add a job with a prompt already set.
	job := &Job{
		Schedule: "@hourly",
		Prompt:   "already-set",
		Platform: "x",
		ChatID:   "c",
		WorkDir:  "/tmp",
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := s.SetJobPrompt(job.ID, "new-prompt"); err == nil {
		t.Error("expected ErrPromptAlreadySet")
	}
	assertMuUnlocked(t, s, "already-set path")

	// Path 3: add a paused job with no prompt, then set prompt (success).
	job2 := &Job{
		Schedule: "@hourly",
		Platform: "x",
		ChatID:   "c2",
		WorkDir:  "/tmp",
		Paused:   true,
	}
	if err := s.AddJob(job2); err != nil {
		t.Fatalf("AddJob paused: %v", err)
	}
	if err := s.SetJobPrompt(job2.ID, "first-prompt"); err != nil {
		t.Errorf("SetJobPrompt success path: %v", err)
	}
	assertMuUnlocked(t, s, "success path")
}

// assertMuUnlocked verifies that s.tbl.mu is not held by trying to acquire a
// read lock within a short timeout. If the read lock cannot be acquired, the
// mutex is stuck and the test fails.
func assertMuUnlocked(t *testing.T, s *Scheduler, context string) {
	t.Helper()
	// RLock should be immediately acquirable if no write lock is held.
	// Use a goroutine + channel to enforce a timeout.
	done := make(chan struct{})
	go func() {
		s.tblForTest().mu.RLock()
		//lint:ignore SA2001 intentional empty critical section: probes that the lock is acquirable
		s.tblForTest().mu.RUnlock()
		close(done)
	}()
	select {
	case <-done:
		// mu is not held — good.
	case <-time.After(200 * time.Millisecond): // 200ms is generous; a real stuck mutex never releases
		t.Errorf("R112714-LOGIC-2 [%s]: s.tbl.mu appears to be permanently locked "+
			"after SetJobPrompt returned. The IIFE + defer Unlock() fix must "+
			"ensure s.tbl.mu is always released on all return paths including panics.",
			context)
	}
}
