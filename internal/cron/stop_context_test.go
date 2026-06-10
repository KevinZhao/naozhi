package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestStopContext_CancelledCtxShortCircuitsDrain pins R250-ARCH-5 (#1168): a
// cancelled shutdown ctx pre-empts the trigger drain budget so StopContext
// returns promptly even when an in-flight TriggerNow goroutine would otherwise
// hold the full stopBudget. This is the Stop(ctx) idiom complement to
// StartContext(ctx).
func TestStopContext_CancelledCtxShortCircuitsDrain(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 5}, SchedulerDeps{})
	// Use a long stopBudget so the test can only pass if the ctx-cancel arm
	// (not the budget timer) is what unblocks the drain.
	withShortStopBudget(t, s, 30*time.Second)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Hold a triggerWG goroutine open so drainTriggerWG would block for the
	// full 30s budget without the ctx cancel.
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	s.triggerWG.Add(1)
	go func() {
		defer s.triggerWG.Done()
		<-hold
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before StopContext runs

	start := time.Now()
	s.StopContext(ctx)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("StopContext with cancelled ctx took %v, want prompt return (#1168)", elapsed)
	}
	if err := s.stopCtx.Err(); err == nil {
		t.Fatal("StopContext must cancel the scheduler stopCtx (#1168)")
	}
}

// TestStopContext_NilCtxBehavesLikeStop pins that StopContext(nil) is
// equivalent to Stop(): no extra cancel arm, drains honour internal budgets,
// and the lifecycle is correctly terminated.
func TestStopContext_NilCtxBehavesLikeStop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(dir, "cron.json"), MaxJobs: 5}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	s.StopContext(nil)

	if err := s.stopCtx.Err(); err == nil {
		t.Fatal("StopContext(nil) must cancel stopCtx like Stop() (#1168)")
	}
	// Idempotent: a second Stop / StopContext is a no-op.
	s.Stop()
}
