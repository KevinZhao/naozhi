package cron

// R247-CR-4 (#584): Stop's 4-stage state machine was extracted into
// waitGCDrain / drainCronStop / drainTriggerWG / persistOnShutdown. These
// tests pin each helper's contract independently so a regression in one
// stage doesn't hide behind another stage's drain.

import (
	"path/filepath"
	"testing"
	"time"
)

// TestPersistOnShutdown_WritesPendingMutation: persistOnShutdown alone
// must land cron_jobs.json — without it, AddJob mutations made up to the
// moment Stop ran would be lost. R246-GO-5 (#690).
func TestPersistOnShutdown_WritesPendingMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")
	s := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 5})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if err := s.AddJob(&Job{
		Schedule: "@every 1h",
		Prompt:   "persist-on-shutdown",
		Platform: "p",
		ChatID:   "c",
		Paused:   true,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Direct invocation: persistOnShutdown is private but in-package, so
	// tests can call it without going through full Stop().
	s.persistOnShutdown()

	loaded, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(loaded) != 1 {
		t.Errorf("loaded jobs = %d, want 1; persistOnShutdown failed to flush", len(loaded))
	}
}

// TestDrainTriggerWG_SkipsBeyondBudget: drainTriggerWG must not pin Stop
// past the remaining budget when triggerWG is held open by a stuck
// notifier. Bound the helper directly with a tight budget.
func TestDrainTriggerWG_SkipsBeyondBudget(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 5})
	withShortStopBudget(t, s, 30*time.Millisecond)

	// Seed a triggerWG holder that outlives the test budget.
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	s.triggerWG.Add(1)
	go func() {
		defer s.triggerWG.Done()
		<-hold
	}()

	// stopStart=now means the helper has the full stopBudget remaining.
	start := time.Now()
	s.drainTriggerWG(nil, start)
	elapsed := time.Since(start)

	// 30ms budget; allow 4× slack for scheduler jitter on slow CI.
	if elapsed > 200*time.Millisecond {
		t.Errorf("drainTriggerWG took %v, want < 200ms", elapsed)
	}
}

// TestDrainTriggerWG_FastDrain: when triggerWG is already empty, the
// helper returns essentially instantly even though stopBudget would
// allow it to wait.
func TestDrainTriggerWG_FastDrain(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 5})
	withShortStopBudget(t, s, 5*time.Second)

	start := time.Now()
	s.drainTriggerWG(nil, start)
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Errorf("empty drainTriggerWG took %v, want < 200ms", elapsed)
	}
}

// TestWaitGCDrain_DoesNotBlockWhenGCEmpty: with no GC goroutine outstanding,
// waitGCDrain returns essentially instantly, well under gcWaitBudget.
func TestWaitGCDrain_DoesNotBlockWhenGCEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 5})

	start := time.Now()
	s.waitGCDrain(nil)
	elapsed := time.Since(start)

	// Empty WaitGroup → close(gcDone) lands before the timer fires.
	if elapsed > 200*time.Millisecond {
		t.Errorf("waitGCDrain took %v, want < 200ms (no gc goroutines)", elapsed)
	}
}

// TestWaitGCDrain_BoundedByBudget: a held-open gcWG cannot pin the
// helper past gcWaitBudget. Confirms the timer arm fires when the
// drain channel doesn't.
func TestWaitGCDrain_BoundedByBudget(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 5})

	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	s.gcWG.Add(1)
	go func() {
		defer s.gcWG.Done()
		<-hold
	}()

	if testing.Short() {
		t.Skip("waitGCDrain budget test waits up to gcWaitBudget=5s; skip in -short")
	}

	start := time.Now()
	s.waitGCDrain(nil)
	elapsed := time.Since(start)

	// Allow a wide upper bound — gcWaitBudget is currently 5s; if a future
	// change shortens it the test still passes. Lower bound proves we
	// actually waited (close to gcWaitBudget) and didn't return early.
	if elapsed > gcWaitBudget+2*time.Second {
		t.Errorf("waitGCDrain took %v, want ≤ gcWaitBudget+2s", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("waitGCDrain returned too quickly (%v); did the timer arm?", elapsed)
	}
}
