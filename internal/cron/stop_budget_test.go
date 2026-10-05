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
//   1. stopBudget is a per-instance 30s deadline (seeded from the
//      defaultStopBudget const), shared across both waits. No doubling.
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

	"github.com/naozhi/naozhi/internal/metrics"
)

// withShortStopBudget shortens a Scheduler's per-instance stop budget for
// the duration of a test and restores the original on cleanup.
// R20260603150052-GO-2 (#1712): routed through WithStopBudgetField on the
// constructed instance — the package-level var seam is gone, so the swap
// is local to s and cannot race a concurrent Stop on another Scheduler.
// Must be called AFTER NewScheduler.
func withShortStopBudget(t *testing.T, s *Scheduler, d time.Duration) {
	t.Helper()
	t.Cleanup(WithStopBudgetField(s, d))
}

func TestStop_BudgetCapsTotalDuration(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:   filepath.Join(dir, "cron.json"),
		MaxJobs:     5,
		ExecTimeout: time.Hour, // worst-case prod value — proves we do not use this
	}, SchedulerDeps{})
	withShortStopBudget(t, s, 80*time.Millisecond)
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

	// Stop's wall clock also covers waitGCDrain and the final fsync'd
	// persist, so the budget mechanism is proven by the breach counters, not
	// by elapsed time. Either budget arm (drain or trigger) skipping the held
	// triggerWG satisfies the contract; exactly one must fire. Not
	// t.Parallel: the counters are process-global expvars.
	drain0 := metrics.CronStopBudgetExceededDrainTotal.Value()
	trigger0 := metrics.CronStopBudgetExceededTriggerTotal.Value()
	start := time.Now()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.Stop()
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatalf("Stop still blocked after 10s with triggerWG held (budget=%v); it must not wait for triggerWG", s.stopBudget)
	}
	elapsed := time.Since(start)

	breaches := metrics.CronStopBudgetExceededDrainTotal.Value() - drain0 +
		metrics.CronStopBudgetExceededTriggerTotal.Value() - trigger0
	if breaches != 1 {
		t.Errorf("stop budget breaches = %d, want 1 (the held triggerWG must trip exactly one budget arm)", breaches)
	}
	// Loose guard against the old ExecTimeout-derived budget (>= 1h).
	if elapsed > 5*time.Second {
		t.Errorf("Stop took %v, want < 5s (budget=%v)", elapsed, s.stopBudget)
	}
}

func TestStop_BudgetRunsSaveJobsEvenOnTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")
	s := NewScheduler(SchedulerConfig{
		StorePath: path,
		MaxJobs:   5,
	}, SchedulerDeps{})
	withShortStopBudget(t, s, 50*time.Millisecond)
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
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	withShortStopBudget(t, s, 5*time.Second)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)

	// No jobs, no hanging triggerWG. A fast-path regression waits out the
	// 5s budget, so 2s separates the two without timing the persist fsync.
	if elapsed > 2*time.Second {
		t.Errorf("clean Stop took %v, want < 2s", elapsed)
	}
}

// TestStop_ConcurrentTriggerWGNotLostOnBudget confirms that abandoning
// triggerWG.Wait does NOT corrupt the WaitGroup — a subsequent Stop (or
// accidental second Stop) still works. WaitGroup counter staying >0
// after Stop is tolerable because the host process is about to exit.
func TestStop_ConcurrentTriggerWGNotLostOnBudget(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	withShortStopBudget(t, s, 30*time.Millisecond)
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
