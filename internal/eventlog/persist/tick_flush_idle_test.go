package persist

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestTickFlush_EmptyWritersSkipsClock pins R250-PERF-6 (#1110): when
// p.writers is empty, tickFlush must short-circuit before calling
// p.opts.Clock(). On idle deployments the run loop's flush ticker fires
// every FlushInterval/2 (default 100ms), so an unguarded tickFlush walks
// an empty map ~10×/s and burns vDSO cycles for nothing. tickIdleClose
// already had the symmetric guard (R249-PERF-20); this test pins the
// new one for tickFlush.
func TestTickFlush_EmptyWritersSkipsClock(t *testing.T) {
	var clockCalls atomic.Int64
	p, _ := newTestPersister(t, func(o *Options) {
		o.Clock = func() time.Time {
			clockCalls.Add(1)
			return time.Unix(1700000000, 0)
		}
	})

	// Drain the startup Clock calls so the assertion below is clean.
	// NewPersister calls Clock during init for sweep + writer bookkeeping;
	// we only care that tickFlush itself doesn't add to the count.
	startCalls := clockCalls.Load()

	// Sanity: no writers active.
	if got := len(p.writers); got != 0 {
		t.Fatalf("expected 0 writers at start, got %d", got)
	}

	// Drive tickFlush directly. It runs on the run goroutine in production;
	// invoking it from the test goroutine is safe for THIS read because the
	// guard short-circuits on len(p.writers)==0 without touching any
	// non-atomic state. If the guard ever drops, this test catches it via
	// either a clockCalls increment (the cheap check) or the race detector
	// (the strict check, when -race is on).
	p.tickFlush()

	if got := clockCalls.Load(); got != startCalls {
		t.Fatalf("tickFlush with no writers must not call Clock: before=%d after=%d",
			startCalls, got)
	}
}
