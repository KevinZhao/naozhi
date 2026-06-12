package cron

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestTriggerNow_RejectsAfterStop pins R20260610-085718-LB-7 (#2012):
// TriggerNow must refuse to register triggerWG work once the scheduler is
// stopped. Stop() sets s.stopped (CAS) before draining triggerWG via
// triggerWG.Wait(); an in-flight HandleTrigger landing triggerWG.Add(1) after
// that Wait would be a positive delta from a zero counter — a sync.WaitGroup
// "Add concurrent with Wait" misuse that can let a trigger goroutine escape
// the drain barrier into router.Shutdown / persistOnShutdown (and, in the
// narrow window, panic). The gate must return ErrSchedulerStopped instead.
func TestTriggerNow_RejectsAfterStop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Register a normal, runnable job so the only reason TriggerNow can fail
	// is the stopped gate — not ErrJobNotFound / Paused / NoPrompt.
	s.mu.Lock()
	s.jobs["job-1"] = &Job{
		ID:       "job-1",
		Schedule: "@every 1h",
		Prompt:   "stub",
	}
	s.mu.Unlock()

	s.Stop()

	err := s.TriggerNow("job-1")
	if !errors.Is(err, ErrSchedulerStopped) {
		t.Fatalf("TriggerNow after Stop = %v; want ErrSchedulerStopped", err)
	}

	// The gate must reject BEFORE triggerWG.Add(1); a leaked reservation would
	// leave the counter non-zero. triggerWG.Wait() returning promptly confirms
	// no slot was added. (Stop already drained any prior work, so the counter
	// is zero unless TriggerNow wrongly added to it.)
	done := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-timeoutAfter(t):
		t.Fatal("triggerWG non-zero after TriggerNow-post-Stop; the stopped gate added a slot it must not have")
	}
}
