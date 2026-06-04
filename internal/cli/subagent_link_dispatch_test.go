package cli

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestDispatchResolve_PoolReused_R214_PERF_6 anchors #415: DispatchResolve
// must hand off to a long-lived worker pool rather than spawning a fresh
// goroutine per call. We can't observe goroutines directly without runtime
// internals, but we can verify the pool is created lazily on first call
// (resolveJobs becomes non-nil) and reused on subsequent calls (the same
// channel handle persists).
func TestDispatchResolve_PoolReused_R214_PERF_6(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()

	// Before first dispatch, pool is not yet started.
	if l.resolveJobs != nil {
		t.Fatal("resolveJobs should be nil before first DispatchResolve (lazy init)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First dispatch lazy-starts the pool. Use empty projectDir so Resolve
	// bails fast inside the worker (we just want to confirm the dispatch
	// path doesn't deadlock or panic).
	l.DispatchResolve(ctx, "task-A", "tu-A", "name", "desc", 0)

	// Pool channel must now exist.
	if l.resolveJobs == nil {
		t.Fatal("resolveJobs should be non-nil after first DispatchResolve")
	}
	first := l.resolveJobs

	// Second dispatch must reuse the same queue (no duplicate Once entry).
	l.DispatchResolve(ctx, "task-B", "tu-B", "name", "desc", 0)
	if l.resolveJobs != first {
		t.Fatal("DispatchResolve must reuse the lazily-initialized pool, not recreate it")
	}
}

// TestDispatchResolve_EmptyTaskIDNoOp_R214_PERF_6 anchors #415: empty taskID
// must short-circuit before the pool starts, so callers that accidentally
// pass empty values don't allocate a queue or workers for nothing.
func TestDispatchResolve_EmptyTaskIDNoOp_R214_PERF_6(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()
	l.DispatchResolve(context.Background(), "", "tu", "name", "desc", 0)
	if l.resolveJobs != nil {
		t.Fatal("empty taskID must not lazy-start the pool")
	}
}

// TestDispatchResolve_QueueFullFallback_R214_PERF_6 anchors the "queue full
// → inline goroutine fallback" branch from the issue's proposal. We force
// the queue to fill by pre-injecting jobs while workers are blocked on a
// sync barrier, then verify the (resolveQueueDepth+1)th call still
// completes without blocking the caller.
//
// The fallback path is observable via worker behaviour: it spawns a one-off
// goroutine that runs Resolve directly (which short-circuits on the empty
// projectDir). We assert non-blocking completion within a tight deadline —
// if DispatchResolve ever blocked on the channel send instead of falling
// back, this test would time out.
func TestDispatchResolve_QueueFullFallback_R214_PERF_6(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()

	// Use a context the workers will block on indefinitely so the queue
	// stays full. The first dispatch starts workers under this ctx; they
	// will pick up jobs but Resolve immediately returns (no projectDir),
	// so to actually fill the queue we need ctx-blocked dispatches.
	//
	// Trick: use a context whose Done is already closed → workers exit
	// immediately on first iteration without consuming the queue. That
	// leaves all subsequent DispatchResolves to fill the queue and then
	// exercise the overflow path.
	deadCtx, cancel := context.WithCancel(context.Background())
	cancel() // Done already closed.

	// First call lazy-starts the pool with deadCtx; workers see ctx.Done
	// and exit immediately without consuming.
	l.DispatchResolve(deadCtx, "task-warmup", "tu", "name", "desc", 0)
	// Give workers a beat to observe the canceled ctx and exit.
	time.Sleep(10 * time.Millisecond)

	// Now fill the queue — resolveQueueDepth slots are available since the
	// workers have all exited.
	for i := 0; i < resolveQueueDepth; i++ {
		// Use a fresh ctx per call so the inline-fallback path (which
		// also runs Resolve) doesn't share the canceled one.
		l.DispatchResolve(context.Background(), "task-fill", "tu", "name", "desc", 0)
	}

	// One more call: must NOT block (queue is full, fallback path fires).
	done := make(chan struct{})
	go func() {
		l.DispatchResolve(context.Background(), "task-overflow", "tu", "name", "desc", 0)
		close(done)
	}()
	select {
	case <-done:
		// Pass: fallback path completed without blocking.
	case <-time.After(2 * time.Second):
		t.Fatal("DispatchResolve blocked on full queue — overflow fallback path did not fire")
	}
}

// TestDispatchResolve_NilCtxSafe anchors that a nil ctx is normalised to
// context.Background() rather than panicking on the worker's select.
func TestDispatchResolve_NilCtxSafe(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()
	// Should not panic.
	l.DispatchResolve(nil, "task-A", "tu", "name", "desc", 0)
	// Smoke test that the pool is alive.
	if l.resolveJobs == nil {
		t.Fatal("nil-ctx DispatchResolve should still lazy-init the pool")
	}
}

// TestDispatchResolve_PoolCtxOutlivesFirstCaller_R20260603030037_GO_2 anchors
// #1661: the worker pool must bind its lifetime to the ctx set via
// SetPoolContext, NOT the per-request ctx of the first DispatchResolve caller.
// We set a long-lived pool ctx, then dispatch first with an already-canceled
// per-request ctx. If the pool wrongly captured the caller's ctx, the workers
// would exit and a follow-up job would never be consumed.
func TestDispatchResolve_PoolCtxOutlivesFirstCaller_R20260603030037_GO_2(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()

	// Long-lived pool ctx, never canceled during the test.
	poolCtx, poolCancel := context.WithCancel(context.Background())
	defer poolCancel()
	l.SetPoolContext(poolCtx)

	// Observe consumption directly via scanHook: a worker that picks up a job
	// runs Resolve, which (after the fast-path stat miss) scans the dir and
	// fires scanHook. Tight timings so the scan path is reached in ms.
	var consumed atomic.Int32
	l.scanHook = func() { consumed.Add(1) }
	l.ConfigureForTest(int64(time.Millisecond), 1, 0) // 1ms retry, 1 retry, no cache
	// Give the linker a projectDir so Resolve does a rawScan (fires scanHook)
	// rather than bailing on empty context.
	dir := t.TempDir()
	l.SetContext(dir, "sess-1")

	// First dispatch uses an ALREADY-CANCELED per-request ctx. Under the old
	// behaviour this canceled ctx became the worker lifetime → workers exit.
	deadCtx, deadCancel := context.WithCancel(context.Background())
	deadCancel()
	l.DispatchResolve(deadCtx, "task-first", "tu-1", "name", "desc", 0)

	// Second dispatch under the live pool — must still be consumed by a worker.
	l.DispatchResolve(context.Background(), "task-second", "tu-2", "name", "desc", 0)

	// Wait for the worker pool to drain at least one job (proving workers are
	// alive despite the first caller's canceled ctx).
	deadline := time.After(2 * time.Second)
	for consumed.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("worker pool exited with first caller's canceled ctx — #1661 regression: no job consumed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSetPoolContext_FirstWins_R20260603030037_GO_2 anchors that SetPoolContext
// is idempotent: the first non-nil ctx sticks, later calls are no-ops.
func TestSetPoolContext_FirstWins_R20260603030037_GO_2(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()
	first, c1 := context.WithCancel(context.Background())
	defer c1()
	second, c2 := context.WithCancel(context.Background())
	defer c2()

	l.SetPoolContext(first)
	l.SetPoolContext(second)

	l.mu.RLock()
	got := l.poolCtx
	l.mu.RUnlock()
	if got != first {
		t.Fatal("SetPoolContext must keep the first ctx; later calls are no-ops")
	}
}

// _ keeps atomic imported even if the suite later drops the only user;
// some test additions in this package's history have churned over this
// import and we want to avoid a re-add cycle.
var _ = atomic.Bool{}
