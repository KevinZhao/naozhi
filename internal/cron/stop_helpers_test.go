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

// TestDrainTriggerWG_SkipsBeyondBudget: drainTriggerWG must not pin Stop
// past the remaining budget when triggerWG is held open by a stuck
// notifier. Bound the helper directly with a tight budget.
func TestDrainTriggerWG_SkipsBeyondBudget(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 5}, SchedulerDeps{})
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
	//lint:ignore SA1012 intentional nil ctx: pins the defensive nil-ctx fallback under test
	s.drainTriggerWG(nil, start)
	elapsed := time.Since(start)

	// 30ms budget; allow 4× slack for scheduler jitter on slow CI.
	if elapsed > 200*time.Millisecond {
		t.Errorf("drainTriggerWG took %v, want < 200ms", elapsed)
	}
}

// TestWaitGCDrain_BoundedByBudget: a held-open gcWG cannot pin the
// helper past gcWaitBudget. Confirms the timer arm fires when the
// drain channel doesn't.
func TestWaitGCDrain_BoundedByBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 5}, SchedulerDeps{})
	// The invariant is "waitGCDrain gives up after ITS budget", not "the budget is
	// 5 seconds". Injecting a short one tests the same give-up path in 200ms
	// instead of 5s, which is what this test spent in every CI run.
	const budget = 200 * time.Millisecond
	t.Cleanup(WithGCBudgetField(s, budget))

	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	s.gcWG.Add(1)
	go func() {
		defer s.gcWG.Done()
		<-hold
	}()

	start := time.Now()
	//lint:ignore SA1012 intentional nil ctx: pins the defensive nil-ctx fallback under test
	s.waitGCDrain(nil)
	elapsed := time.Since(start)

	// Both bounds are relative to the INJECTED budget, so the test asserts the
	// give-up behaviour rather than a particular duration. The generous upper slack
	// absorbs CI scheduling jitter; the lower bound is the load-bearing half — it
	// proves the timer armed at all instead of waitGCDrain returning immediately.
	if elapsed > budget+2*time.Second {
		t.Errorf("waitGCDrain took %v, want <= injected budget %v + 2s", elapsed, budget)
	}
	if elapsed < budget/2 {
		t.Errorf("waitGCDrain returned in %v, well under the %v budget; did the timer arm?", elapsed, budget)
	}
}
