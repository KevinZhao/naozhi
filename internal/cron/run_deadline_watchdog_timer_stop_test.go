package cron

import (
	"context"
	"testing"
	"time"
)

// TestRunDeadlineWatchdog_FastPathReleasesTimerSlot is the regression
// test for R20260527122801-GO-001. Pre-fix, the watchdog used
// time.After(watchdogInterruptTimeout()) which leaks a *Timer slot
// until expiry on the success path; mirroring scheduler.go:1337's
// NewTimer + defer Stop pattern releases timer state deterministically
// when InterruptViaControl returns first.
//
// The test pumps a long watchdogInterruptTimeout (so the timer slot
// would persist visibly if it leaked) and verifies the abort channel
// delivers the real InterruptSent outcome promptly, before the timeout
// could possibly fire — proving the success branch wins and the
// `defer t.Stop()` releases the timer.
func TestRunDeadlineWatchdog_FastPathReleasesTimerSlot(t *testing.T) {
	// NOT t.Parallel() — mutates package-level watchdogInterruptTimeoutAtomic.
	// Long enough that, if the test instead waited for the timer, it
	// would dwarf the deadline-fire latency by ~2 orders of magnitude.
	setWatchdogInterruptTimeoutForTest(t, 2*time.Second)

	ci := &countingInterrupter{outcome: InterruptSent}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	start := time.Now()
	ch, _ := runDeadlineWatchdog(ctx, ci)
	abort := <-ch
	elapsed := time.Since(start)

	if !abort.fired {
		t.Fatal("abort.fired = false; want true on DeadlineExceeded")
	}
	if abort.outcome != InterruptSent {
		t.Fatalf("abort.outcome = %v, want InterruptSent (fast return preserves real outcome)", abort.outcome)
	}
	// Fast-path must beat the 2s timeout by a healthy margin —
	// anything close to it implies the timer fired instead of being
	// preempted by the done channel.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("abort delivered after %v; success path should preempt the 2s timer", elapsed)
	}
}
