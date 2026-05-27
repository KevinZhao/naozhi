package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunHistoryTask_SkipsAfterCancel pins the late-Add(1) invariant for
// the R222-ARCH-17 (#748) helper: once historyCtx is cancelled, new tasks
// must be refused rather than spawned past historyWg.Wait().
func TestRunHistoryTask_SkipsAfterCancel(t *testing.T) {
	r := &Router{}
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())
	r.historyCancel()

	var ran atomic.Bool
	if r.runHistoryTask(func(_ context.Context) { ran.Store(true) }) {
		t.Fatal("runHistoryTask returned true after historyCtx cancelled — must refuse")
	}
	// runHistoryTask returned false synchronously, no goroutine launched.
	r.historyWg.Wait()
	if ran.Load() {
		t.Fatal("task body ran after historyCtx was cancelled")
	}
}

// TestRunHistoryTask_RunsAndPropagatesCtx confirms the happy path: the
// goroutine runs, sees the historyCtx, and historyWg.Wait blocks until
// it returns.
func TestRunHistoryTask_RunsAndPropagatesCtx(t *testing.T) {
	r := &Router{}
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())
	defer r.historyCancel()

	done := make(chan struct{})
	var sawCtx atomic.Bool
	if !r.runHistoryTask(func(ctx context.Context) {
		// Confirm we got the historyCtx, not Background.
		if ctx == r.historyCtx {
			sawCtx.Store(true)
		}
		close(done)
	}) {
		t.Fatal("runHistoryTask returned false on a live historyCtx")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task did not run within 1s")
	}
	r.historyWg.Wait() // must not deadlock — Done was deferred inside runHistoryTask
	if !sawCtx.Load() {
		t.Fatal("task body did not receive r.historyCtx")
	}
}

// TestRunHistoryTask_NilCtxFallsBackToBackground covers test-Router
// construction (struct literal, no NewRouter) where historyCtx is nil.
// The helper must still spawn — otherwise unit tests of subsystems that
// adopt this pattern silently no-op.
func TestRunHistoryTask_NilCtxFallsBackToBackground(t *testing.T) {
	r := &Router{} // historyCtx left nil

	done := make(chan struct{})
	if !r.runHistoryTask(func(ctx context.Context) {
		if ctx == nil {
			t.Error("nil-historyCtx fallback passed nil ctx to fn; want context.Background")
		}
		close(done)
	}) {
		t.Fatal("runHistoryTask returned false despite nil historyCtx")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task did not run within 1s")
	}
	r.historyWg.Wait()
}
