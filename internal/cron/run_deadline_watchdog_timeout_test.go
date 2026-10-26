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
	// returned is closed by InterruptViaControl just before it returns so
	// tests can deterministically wait for the parked inner goroutine to
	// drain.
	returned chan struct{}
}

func (b *blockingInterrupter) InterruptViaControl() InterruptOutcome {
	b.calls.Add(1)
	<-b.release
	if b.returned != nil {
		close(b.returned)
	}
	return b.outcome
}

// waitDrained blocks until the inner InterruptViaControl goroutine has
// returned (release must already be closed).
func (b *blockingInterrupter) waitDrained(t *testing.T) {
	t.Helper()
	if b.returned == nil {
		return
	}
	select {
	case <-b.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("blockingInterrupter inner goroutine never returned after release")
	}
}

// TestRunDeadlineWatchdog_TimeoutOnWedgedInterrupt is the regression test
// for R236-GO-09 (#507). Pre-fix, a wedged InterruptViaControl held the
// watchdog goroutine forever; the caller's `<-abortCh` blocked, and
// finishRun was never invoked, leaving inflight.running=true so every
// subsequent tick silently skipped the job until process restart.
//
// The test passes a short interrupt timeout instead of the production
// watchdogInterruptTimeoutDefault so we don't burn 3s in CI, asserts abort is delivered
// promptly with fired=true and outcome=InterruptError, and finally
// releases the stub so the inner goroutine drains cleanly.
func TestRunDeadlineWatchdog_TimeoutOnWedgedInterrupt(t *testing.T) {
	// R20260607-GO-4 (#1904): timeout is now a per-call parameter, so this test
	// is fully isolated and may run in parallel.
	t.Parallel()

	bi := &blockingInterrupter{release: make(chan struct{}), returned: make(chan struct{}), outcome: InterruptSent}
	defer bi.waitDrained(t) // ensure the parked goroutine drains before returning
	defer close(bi.release) // unblock the inner goroutine before the test ends

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	ch, _ := runDeadlineWatchdog(ctx, bi, 50*time.Millisecond)

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
	// R20260607-GO-4 (#1904): timeout is a per-call parameter, so this test is
	// isolated and may run in parallel.
	t.Parallel()

	ci := &countingInterrupter{outcome: InterruptUnsupported}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	chUnsup, _ := runDeadlineWatchdog(ctx, ci, 200*time.Millisecond)
	abort := <-chUnsup
	if !abort.fired {
		t.Fatal("abort.fired = false; want true on DeadlineExceeded")
	}
	if abort.outcome != InterruptUnsupported {
		t.Fatalf("abort.outcome = %v, want InterruptUnsupported (fast return preserves real outcome)", abort.outcome)
	}
}
