package cron

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// blockingInterrupter is a deadlineInterrupter test stub that blocks
// InterruptViaControl until release is closed. Mirrors the wedged
// session.InterruptViaControl scenario where the control_request
// channel never gets acked because stdin is kernel-blocked.
type blockingInterrupter struct {
	release chan struct{}
	calls   atomic.Int32
	outcome InterruptOutcome
}

func (b *blockingInterrupter) InterruptViaControl() InterruptOutcome {
	b.calls.Add(1)
	<-b.release
	return b.outcome
}

// TestRunDeadlineWatchdog_TimeoutOnWedgedInterrupt is the regression test
// for R236-GO-09 (#507). Pre-fix, a wedged InterruptViaControl held the
// watchdog goroutine forever; the caller's `<-abortCh` blocked, and
// finishRun was never invoked, leaving inflight.running=true so every
// subsequent tick silently skipped the job until process restart.
//
// The test substitutes the production watchdogInterruptTimeout for a
// short value so we don't burn 3s in CI, asserts abort is delivered
// promptly with fired=true and outcome=InterruptError, and finally
// releases the stub so the inner goroutine drains cleanly.
func TestRunDeadlineWatchdog_TimeoutOnWedgedInterrupt(t *testing.T) {
	// NOT t.Parallel() — mutates package-level watchdogInterruptTimeoutAtomic.

	prev := watchdogInterruptTimeoutAtomic.Load()
	watchdogInterruptTimeoutAtomic.Store(int64(50 * time.Millisecond))
	defer watchdogInterruptTimeoutAtomic.Store(prev)

	bi := &blockingInterrupter{release: make(chan struct{}), outcome: InterruptSent}
	defer close(bi.release) // unblock the inner goroutine before the test ends

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	ch := runDeadlineWatchdog(ctx, bi)

	select {
	case abort := <-ch:
		if !abort.fired {
			t.Fatalf("abort.fired = false, want true (timeout still counts as an attempt)")
		}
		if abort.outcome != InterruptError {
			t.Fatalf("abort.outcome = %v, want InterruptError on watchdog timeout", abort.outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abortCh blocked beyond watchdog timeout; finishRun would never be called")
	}

	if got := bi.calls.Load(); got != 1 {
		t.Fatalf("InterruptViaControl call count = %d, want 1", got)
	}
}

// TestRunDeadlineWatchdog_FastInterruptStillWins asserts the timeout
// path does NOT fire when InterruptViaControl returns promptly — the
// real outcome is preserved, not overridden to InterruptError.
func TestRunDeadlineWatchdog_FastInterruptStillWins(t *testing.T) {
	// NOT t.Parallel() — mutates package-level watchdogInterruptTimeoutAtomic.

	prev := watchdogInterruptTimeoutAtomic.Load()
	watchdogInterruptTimeoutAtomic.Store(int64(200 * time.Millisecond))
	defer watchdogInterruptTimeoutAtomic.Store(prev)

	ci := &countingInterrupter{outcome: InterruptUnsupported}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	abort := <-runDeadlineWatchdog(ctx, ci)
	if !abort.fired {
		t.Fatal("abort.fired = false; want true on DeadlineExceeded")
	}
	if abort.outcome != InterruptUnsupported {
		t.Fatalf("abort.outcome = %v, want InterruptUnsupported (fast return preserves real outcome)", abort.outcome)
	}
}
