package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestStopCtx_ParentCancelPropagates pins R249-ARCH-8 (#974): stopCtx is the
// single authoritative cancel signal. A SchedulerConfig.ParentCtx cancel and
// an explicit Stop() are not independent paths — both converge on stopCtx,
// because stopCtx is derived from ParentCtx via context.WithCancel. This test
// proves the ParentCtx half: cancelling the parent must cancel stopCtx so
// every in-flight reader observing stopCtx.Done() reacts to host shutdown
// without the scheduler needing to read ParentCtx after NewScheduler.
func TestStopCtx_ParentCancelPropagates(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.Background())
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(t.TempDir(), "cron.json"),
		MaxJobs:   10,
		ParentCtx: parent,
	}, SchedulerDeps{})

	if err := s.stopCtx.Err(); err != nil {
		t.Fatalf("stopCtx must be live before any cancel, got %v", err)
	}

	cancelParent()

	select {
	case <-s.stopCtx.Done():
		// expected: parent cancel propagated into the derived stopCtx.
	case <-time.After(time.Second):
		t.Fatal("ParentCtx cancel did not propagate to stopCtx within 1s — " +
			"stopCtx is no longer derived from ParentCtx (R249-ARCH-8 / #974)")
	}
}

// TestStopCtx_StopCancels pins the other convergence half: an explicit Stop()
// cancels the same stopCtx, with no dependence on ParentCtx (default parent).
func TestStopCtx_StopCancels(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(t.TempDir(), "cron.json"),
		MaxJobs:   10,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := s.stopCtx.Err(); err != nil {
		t.Fatalf("stopCtx must be live before Stop, got %v", err)
	}

	s.Stop()

	if err := s.stopCtx.Err(); err == nil {
		t.Fatal("Stop() must cancel stopCtx — the authoritative cancel signal (#974)")
	}
}
