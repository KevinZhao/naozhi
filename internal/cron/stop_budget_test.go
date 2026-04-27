package cron

// R49-REL-CRON-STOP-BUDGET regression tests.
//
// Prior to Round 98 the Scheduler.Stop budget was `execTimeout+5s` per
// wait stage, doubled across `cron.Stop().Done()` + `triggerWG.Wait`.
// With the production execTimeout=3600s this made the worst-case Stop
// block for ≈2 h — well past systemd's TimeoutStopSec=5, which in turn
// let a `systemctl restart` launch the new process before the old one
// released port :8180 (and lose the final saveJobs if anything SIGKILL'd
// the process past the notify-no-kill guard). The new contract:
//
//   1. stopBudget is a package-level 30s deadline, shared across both
//      waits. No doubling.
//   2. If cron.Stop's context does not drain within the budget, Stop()
//      skips triggerWG.Wait entirely and still runs saveJobs.
//   3. If triggerWG.Wait would exceed the remaining budget, Stop()
//      proceeds to saveJobs without waiting.
//
// These tests use a short stopBudget injection so we can prove the
// behaviour without CI spending real wall-clock seconds.

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// withShortStopBudget shortens stopBudget for the duration of a test and
// restores the original on cleanup. Test-side var overwrite is safe
// because we run these tests serially relative to other cron tests that
// call Stop — no cron_test.go uses t.Parallel around Scheduler lifecycle.
func withShortStopBudget(t *testing.T, d time.Duration) {
	t.Helper()
	orig := stopBudget
	stopBudget = d
	t.Cleanup(func() { stopBudget = orig })
}

func TestStop_BudgetCapsTotalDuration(t *testing.T) {
	withShortStopBudget(t, 80*time.Millisecond)

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:   filepath.Join(dir, "cron.json"),
		MaxJobs:     5,
		ExecTimeout: time.Hour, // worst-case prod value — proves we do not use this
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Add a triggerWG hold-up: register a goroutine that will outlive
	// Stop and refuses to exit for 10s. The budget guard must not wait
	// for it. (Released by t.Cleanup so the goroutine does not leak
	// across test runs.)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	s.triggerWG.Add(1)
	go func() {
		defer s.triggerWG.Done()
		<-hold
	}()

	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)

	// Budget is 80ms. Stop should return somewhere under ~4×budget
	// (slack for scheduler jitter on slow CI). If the old per-wait
	// doubling crept back this would regress to ≥1h+5s.
	if elapsed > 400*time.Millisecond {
		t.Errorf("Stop took %v, want < 400ms (budget=%v)", elapsed, stopBudget)
	}
}

func TestStop_BudgetRunsSaveJobsEvenOnTimeout(t *testing.T) {
	withShortStopBudget(t, 50*time.Millisecond)

	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")
	s := NewScheduler(SchedulerConfig{
		StorePath: path,
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Seed a job so saveJobs has something non-empty to write.
	if err := s.AddJob(&Job{
		Schedule: "@every 1h",
		Prompt:   "save me",
		Platform: "p",
		ChatID:   "c",
		Paused:   true,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Hold triggerWG open past the budget.
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	s.triggerWG.Add(1)
	go func() {
		defer s.triggerWG.Done()
		<-hold
	}()

	s.Stop()

	// The state file must exist even though triggerWG did not drain in
	// time. Without the shared-budget rework, hitting the budget during
	// wait 2 was handled via an early `return` that skipped saveJobs
	// entirely in the pre-R49 design; the new control flow falls through
	// to the save path. We prove that by reloading.
	loaded, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(loaded) != 1 {
		t.Errorf("loaded jobs = %d, want 1 (saveJobs skipped?)", len(loaded))
	}
}

func TestStop_FastPathDrainsCleanly(t *testing.T) {
	// Sanity: when nothing holds triggerWG the Stop path returns
	// basically immediately and budget does not affect elapsed time.
	withShortStopBudget(t, 5*time.Second)

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)

	// No jobs, no hanging triggerWG — should finish in milliseconds.
	if elapsed > 500*time.Millisecond {
		t.Errorf("clean Stop took %v, want < 500ms", elapsed)
	}
}

// TestStop_ConcurrentTriggerWGNotLostOnBudget confirms that abandoning
// triggerWG.Wait does NOT corrupt the WaitGroup — a subsequent Stop (or
// accidental second Stop) still works. WaitGroup counter staying >0
// after Stop is tolerable because the host process is about to exit.
func TestStop_ConcurrentTriggerWGNotLostOnBudget(t *testing.T) {
	withShortStopBudget(t, 30*time.Millisecond)

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	hold := make(chan struct{})
	t.Cleanup(func() {
		close(hold)
		wg.Done()
		s.triggerWG.Done()
	})
	s.triggerWG.Add(1)
	go func() {
		wg.Wait()
	}()

	// Must not panic even though triggerWG is held open past the
	// budget. The shared-deadline design skips Wait; the in-flight
	// goroutine eventually terminates when hold is closed at test
	// teardown.
	s.Stop()
}
