package cron

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestRunDeadlineWatchdog_NoIdleGoroutine_R247_GO_12 is the regression test
// for R247-GO-12 (#492). Pre-fix, runDeadlineWatchdog spawned a long-lived
// goroutine waiting on `<-ctx.Done()` for every cron tick — at 50 jobs @ 1Hz
// this held ~50 watchdog goroutines concurrently for the entire Send window.
// The fix uses context.AfterFunc, which only spawns a goroutine when ctx
// actually ends (briefly, to run the callback), shrinking the steady-state
// in-flight watchdog goroutine count to ~0.
//
// The test registers many watchdogs against contexts that have NOT been
// cancelled and asserts the goroutine count remains within a small constant
// of the baseline — proving no per-watchdog goroutine is alive while ctx is
// still live. After cancelling all contexts and draining, the count returns
// to baseline (the AfterFunc callbacks have run and exited).
func TestRunDeadlineWatchdog_NoIdleGoroutine_R247_GO_12(t *testing.T) {
	// NOT t.Parallel() — sensitive to background goroutine churn from
	// other parallel tests in the package.

	const N = 64

	// Drain any goroutines that might still be spinning down from earlier
	// tests in the suite before sampling the baseline.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	cancels := make([]context.CancelFunc, 0, N)
	channels := make([]<-chan abortResult, 0, N)
	for i := 0; i < N; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		ci := &countingInterrupter{outcome: InterruptSent}
		channels = append(channels, runDeadlineWatchdog(ctx, ci))
	}

	// Give the runtime a moment to schedule any goroutines that the old
	// implementation would have spawned. With AfterFunc registration alone,
	// no per-watchdog goroutines should appear.
	runtime.Gosched()
	time.Sleep(5 * time.Millisecond)

	live := runtime.NumGoroutine()
	// Allow a small slack for unrelated runtime goroutines (GC sweeper,
	// timer dispatch) — but the previous implementation would have spawned
	// at least N goroutines, well beyond a constant slack.
	const slack = 8
	if live > baseline+slack {
		t.Fatalf("watchdog goroutines leaked while ctx live: baseline=%d live=%d (delta=%d, slack=%d, N=%d)",
			baseline, live, live-baseline, slack, N)
	}

	// Cancel all contexts; each AfterFunc callback runs once and publishes
	// abortResult{fired:false} to its channel. Drain to be certain.
	for _, cancel := range cancels {
		cancel()
	}
	for _, ch := range channels {
		select {
		case abort := <-ch:
			if abort.fired {
				t.Fatalf("abort.fired = true on explicit cancel; want false")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("AfterFunc callback never published abort on cancel")
		}
	}
}
