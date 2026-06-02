package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestStartContext_CtxCancelPropagates pins R250-ARCH-5 (#1168): the idiomatic
// StartContext(ctx) entry point wires the supplied ctx so its cancellation
// propagates into stopCtx exactly like SchedulerConfig.ParentCtx does. A
// caller can thread the app ctx without constructing a SchedulerConfig literal
// just to set ParentCtx.
func TestStartContext_CtxCancelPropagates(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(t.TempDir(), "cron.json"),
		MaxJobs:   10,
	})
	t.Cleanup(s.Stop)

	if err := s.StartContext(ctx); err != nil {
		t.Fatalf("StartContext: %v", err)
	}
	if err := s.stopCtx.Err(); err != nil {
		t.Fatalf("stopCtx must be live before ctx cancel, got %v", err)
	}

	cancel()

	select {
	case <-s.stopCtx.Done():
		// expected: ctx cancel propagated into stopCtx via the watcher.
	case <-time.After(time.Second):
		t.Fatal("StartContext(ctx) cancel did not propagate to stopCtx within 1s (#1168)")
	}
}

// TestStartContext_NilCtxBehavesLikeStart pins that a nil ctx is equivalent to
// calling Start() directly: the scheduler starts and only Stop() cancels
// stopCtx.
func TestStartContext_NilCtxBehavesLikeStart(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(t.TempDir(), "cron.json"),
		MaxJobs:   10,
	})

	//nolint:staticcheck // intentionally passing nil ctx to exercise the no-watcher path.
	if err := s.StartContext(nil); err != nil {
		t.Fatalf("StartContext(nil): %v", err)
	}
	if err := s.stopCtx.Err(); err != nil {
		t.Fatalf("stopCtx must be live after StartContext(nil), got %v", err)
	}

	s.Stop()

	if err := s.stopCtx.Err(); err == nil {
		t.Fatal("Stop() must cancel stopCtx after StartContext(nil) (#1168)")
	}
}

// TestStartContext_WatcherExitsOnStop pins that the watcher goroutine drains on
// stopCtx (not just ctx) so it cannot leak past Stop() when ctx is never
// cancelled.
func TestStartContext_WatcherExitsOnStop(t *testing.T) {
	t.Parallel()

	// ctx stays live for the whole test; only Stop() fires.
	ctx := context.Background()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(t.TempDir(), "cron.json"),
		MaxJobs:   10,
	})
	if err := s.StartContext(ctx); err != nil {
		t.Fatalf("StartContext: %v", err)
	}

	s.Stop()

	if err := s.stopCtx.Err(); err == nil {
		t.Fatal("Stop() must cancel stopCtx — the watcher's drain signal (#1168)")
	}
}
