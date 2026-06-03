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

// TestRunHistoryTask_CancelledPathDoesNotTouchWaitGroup pins the
// R20260603-CODE-3 (#1655) fix: when historyCtx is already cancelled the
// helper must NOT do a transient historyWg.Add(1)+Done(). We assert this by
// re-using the same WaitGroup across an already-drained Wait — under the old
// add-then-compensate shape the late Add could re-add to a WaitGroup whose
// Wait had already returned and panic with "WaitGroup is reused before
// previous Wait has returned". With the fix the cancelled call is a pure
// no-op on the counter, so the sequence is safe.
func TestRunHistoryTask_CancelledPathDoesNotTouchWaitGroup(t *testing.T) {
	r := &Router{}
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())

	// Drain the WaitGroup once so a stray Add() after this point would be a
	// reuse-before-Wait violation.
	r.historyWg.Wait()

	r.historyCancel()

	// Refused spawn must not Add to (and then Done) the freshly-drained WG.
	// If runHistoryTask did Add(1) here it would either panic on reuse or
	// leave the counter non-zero; the assertion below catches a leak and the
	// recover() catches a reuse panic.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("runHistoryTask touched the drained WaitGroup on the cancelled path: %v", rec)
		}
	}()

	if r.runHistoryTask(func(_ context.Context) {}) {
		t.Fatal("runHistoryTask returned true after historyCtx cancelled — must refuse")
	}
	// Must not block: counter must still be zero (no transient Add leaked).
	r.historyWg.Wait()
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
