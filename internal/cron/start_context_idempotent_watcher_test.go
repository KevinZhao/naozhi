package cron

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
)

// TestStartContext_RepeatCallDoesNotAccumulateGoroutines pins
// R20260603150052-CR-1: calling StartContext(ctx) a second time on an already-
// started scheduler must NOT spawn a second watcher goroutine. Before the fix
// each call unconditionally spawned a goroutine regardless of started state.
//
// The test captures runtime.NumGoroutine() before and after the repeat call and
// asserts no new goroutines remain after a small GC / schedule yield. A single
// watcher goroutine (from the first StartContext) is expected and is cleaned up
// by s.Stop().
func TestStartContext_RepeatCallDoesNotAccumulateGoroutines(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(t.TempDir(), "cron.json"),
		MaxJobs:   10,
	})
	t.Cleanup(s.Stop)

	if err := s.StartContext(ctx); err != nil {
		t.Fatalf("first StartContext: %v", err)
	}

	// Allow the first watcher goroutine to settle.
	runtime.Gosched()

	baseline := runtime.NumGoroutine()

	// Call StartContext again — must be a no-op, no new goroutine.
	if err := s.StartContext(ctx); err != nil {
		t.Fatalf("second StartContext: %v", err)
	}
	runtime.Gosched()

	after := runtime.NumGoroutine()
	if after > baseline {
		t.Fatalf("goroutine count grew from %d to %d after repeat StartContext — watcher goroutine leaked (R20260603150052-CR-1)",
			baseline, after)
	}
}
