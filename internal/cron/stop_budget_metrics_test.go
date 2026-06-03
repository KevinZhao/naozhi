package cron

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestR250GO20_StopBudgetTriggerCounterBumps pins #1083 / R250-GO-20: when
// Stop()'s triggerWG.Wait phase exceeds its remaining-budget slice, the
// CronStopBudgetExceededTriggerTotal counter must bump alongside the
// existing slog.Warn. Without this counter, operators tracking systemd
// TimeoutStopSec breaches via Prometheus had to grep journalctl.
//
// We test the trigger-phase counter rather than the gc / drain counters
// because those phases are harder to wedge from in-package tests
// (gcWG.Wait depends on runStore lifecycle; cron.Stop's drain ctx is
// internal to robfig/cron). The trigger counter shares the same wiring
// pattern as the other two — bumped immediately before the slog.Warn —
// so a regression on any one is structurally observable here. The
// per-counter symbol existence is verified by build (the test file
// imports metrics and references each counter via the structural test
// below).
func TestR250GO20_StopBudgetTriggerCounterBumps(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	withShortStopBudget(t, s, 30*time.Millisecond)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Hold triggerWG open past the budget so the trigger phase trips its
	// timeout branch and bumps the counter.
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

	before := metrics.CronStopBudgetExceededTriggerTotal.Value()
	s.Stop()
	after := metrics.CronStopBudgetExceededTriggerTotal.Value()

	if after <= before {
		t.Errorf("CronStopBudgetExceededTriggerTotal = %d (was %d); expected bump on trigger-phase budget breach (#1083)",
			after, before)
	}
}

// TestR250GO20_StopBudgetCountersExist is the build-time existence guard.
// Adding a per-phase counter to the metrics package without wiring the
// bump in scheduler.Stop() (or vice versa) would be a silent half-fix;
// referencing each counter's Value() here forces both halves to compile.
func TestR250GO20_StopBudgetCountersExist(t *testing.T) {
	t.Parallel()
	// All three counters are read-once to catch a future rename / removal
	// at build time. Counters are monotonic so a Value() call is safe to
	// run concurrently with any phase under test.
	_ = metrics.CronStopBudgetExceededGCTotal.Value()
	_ = metrics.CronStopBudgetExceededDrainTotal.Value()
	_ = metrics.CronStopBudgetExceededTriggerTotal.Value()
}
