package cron

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
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
	// drain (and its gauge decrement to land) instead of leaving async
	// residue that races sibling gauge assertions (R20260602-GO-005).
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
// returned (release must already be closed). It does not guarantee the
// gauge decrement in runDeadlineWatchdog's inner goroutine has landed —
// that decrement runs after InterruptViaControl returns — so callers that
// assert on the gauge should poll, but waitDrained bounds the window.
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
// The test substitutes the production watchdogInterruptTimeout for a
// short value so we don't burn 3s in CI, asserts abort is delivered
// promptly with fired=true and outcome=InterruptError, and finally
// releases the stub so the inner goroutine drains cleanly.
func TestRunDeadlineWatchdog_TimeoutOnWedgedInterrupt(t *testing.T) {
	// NOT t.Parallel() — mutates package-level watchdogInterruptTimeoutAtomic.
	setWatchdogInterruptTimeoutForTest(t, 50*time.Millisecond)

	bi := &blockingInterrupter{release: make(chan struct{}), returned: make(chan struct{}), outcome: InterruptSent}
	defer bi.waitDrained(t) // ensure the parked goroutine drains before returning
	defer close(bi.release) // unblock the inner goroutine before the test ends

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	ch, _ := runDeadlineWatchdog(ctx, bi)

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

// TestRunDeadlineWatchdog_TimeoutBumpsMetric is the regression test for
// R20260527122801-SEC-3 (#1327). The watchdog timeout branch publishes
// CronWatchdogInterruptTimeoutTotal so operators can alert on wedged
// InterruptViaControl events; pre-fix the timeout fired silently and the
// only signal was a slow-rising goroutine count. NOT t.Parallel() —
// mutates the package-level watchdogInterruptTimeoutAtomic and reads the
// shared metric counter (which other tests may also touch but only via
// strict deltas around this test).
func TestRunDeadlineWatchdog_TimeoutBumpsMetric(t *testing.T) {
	setWatchdogInterruptTimeoutForTest(t, 50*time.Millisecond)

	before := metrics.CronWatchdogInterruptTimeoutTotal.Value()

	bi := &blockingInterrupter{release: make(chan struct{}), returned: make(chan struct{}), outcome: InterruptSent}
	defer bi.waitDrained(t)
	defer close(bi.release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	chBI, _ := runDeadlineWatchdog(ctx, bi)
	abort := <-chBI
	if !abort.fired || abort.outcome != InterruptError {
		t.Fatalf("abort = {fired:%v outcome:%v}, want {fired:true outcome:InterruptError}",
			abort.fired, abort.outcome)
	}

	after := metrics.CronWatchdogInterruptTimeoutTotal.Value()
	if got := after - before; got != 1 {
		t.Fatalf("CronWatchdogInterruptTimeoutTotal delta = %d, want 1 (single timeout event)", got)
	}
}

// TestRunDeadlineWatchdog_ParkedGaugeTracksLiveLeak is the regression
// test for R20260602-GO-005 (#1632). When the watchdog times out it must
// bump the LIVE parked-goroutine gauge (so a persistent never-reset job's
// permanent leak is observable as a rising current value), and when the
// wedged InterruptViaControl finally unblocks the inner goroutine must
// decrement the gauge back to baseline. NOT t.Parallel() — mutates
// package-level watchdogInterruptTimeoutAtomic and the shared gauge.
func TestRunDeadlineWatchdog_ParkedGaugeTracksLiveLeak(t *testing.T) {
	setWatchdogInterruptTimeoutForTest(t, 50*time.Millisecond)

	// returned + waitDrained guarantee THIS test's parked inner goroutine
	// (and its gauge -1) drains before the test returns, so it leaves no
	// residue for the next test's baseline. release is closed below once we
	// have observed the park.
	bi := &blockingInterrupter{release: make(chan struct{}), returned: make(chan struct{}), outcome: InterruptSent}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	// Settle the global gauge before capturing the baseline: a sibling
	// watchdog test's parked goroutine may still be running its async
	// Add(-1) decrement (it lands AFTER InterruptViaControl returns, so even
	// that test's waitDrained does not fence it). If that lagging -1 lands
	// between our base read and our own +1, the net is flat and the up-loop
	// below never reaches base+1 — the false "never reached baseline+1"
	// timeout this test used to flake on. Waiting for a stable value pins an
	// honest baseline.
	base := settleParkedGauge()
	chBase, _ := runDeadlineWatchdog(ctx, bi)
	abort := <-chBase
	if !abort.fired || abort.outcome != InterruptError {
		t.Fatalf("abort = {fired:%v outcome:%v}, want {fired:true outcome:InterruptError}",
			abort.fired, abort.outcome)
	}

	// Inner goroutine is still parked on the blocking interrupter: the
	// live gauge must reach at least baseline+1 (this run's park).
	upDeadline := time.After(2 * time.Second)
	for watchdogParkedInterruptGoroutines.Value() < base+1 {
		select {
		case <-upDeadline:
			t.Fatalf("parked gauge never reached baseline+1 while wedged; got baseline%+d",
				watchdogParkedInterruptGoroutines.Value()-base)
		case <-time.After(2 * time.Millisecond):
		}
	}

	// Release the wedged write; the inner goroutine returns and must
	// decrement the gauge back below the parked level. Our run's -1 brings
	// the gauge to its pre-park value (modulo sibling activity, which only
	// trends downward as their goroutines drain too).
	close(bi.release)
	bi.waitDrained(t) // fence the inner goroutine's return before asserting

	downDeadline := time.After(2 * time.Second)
	for watchdogParkedInterruptGoroutines.Value() > base {
		select {
		case <-downDeadline:
			t.Fatalf("parked gauge did not drop back to baseline after release; got baseline%+d",
				watchdogParkedInterruptGoroutines.Value()-base)
		case <-time.After(2 * time.Millisecond):
		}
	}

	if got := bi.calls.Load(); got != 1 {
		t.Fatalf("InterruptViaControl call count = %d, want 1", got)
	}
}

// settleParkedGauge waits until watchdogParkedInterruptGoroutines reports the
// same value across several consecutive reads and returns it. A sibling
// watchdog test's parked inner goroutine decrements the gauge asynchronously
// after InterruptViaControl returns, so a freshly-read baseline can still be
// inflated by a decrement that has not yet landed. Settling first pins an
// honest baseline so the +1/-1 swing this test asserts is not cancelled out.
func settleParkedGauge() int64 {
	prev := watchdogParkedInterruptGoroutines.Value()
	stable := 0
	for i := 0; i < 200; i++ {
		time.Sleep(5 * time.Millisecond)
		n := watchdogParkedInterruptGoroutines.Value()
		if n == prev {
			if stable++; stable >= 3 {
				return n
			}
			continue
		}
		prev = n
		stable = 0
	}
	return prev
}

// TestRunDeadlineWatchdog_FastInterruptLeavesGaugeUntouched asserts the
// live parked gauge is NOT bumped when InterruptViaControl returns before
// the watchdog fires — the CAS(0→2) loses to the inner goroutine's
// CAS(0→1) so no increment happens. R20260602-GO-005 (#1632).
func TestRunDeadlineWatchdog_FastInterruptLeavesGaugeUntouched(t *testing.T) {
	setWatchdogInterruptTimeoutForTest(t, 200*time.Millisecond)

	base := watchdogParkedInterruptGoroutines.Value()

	ci := &countingInterrupter{outcome: InterruptSent}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	chCI, _ := runDeadlineWatchdog(ctx, ci)
	abort := <-chCI
	if !abort.fired || abort.outcome != InterruptSent {
		t.Fatalf("abort = {fired:%v outcome:%v}, want {fired:true outcome:InterruptSent}",
			abort.fired, abort.outcome)
	}

	// Give any stray decrement a moment to surface, then assert baseline.
	time.Sleep(20 * time.Millisecond)
	if got := watchdogParkedInterruptGoroutines.Value() - base; got != 0 {
		t.Fatalf("parked gauge delta on fast return = %d, want 0", got)
	}
}

// TestRunDeadlineWatchdog_FastInterruptStillWins asserts the timeout
// path does NOT fire when InterruptViaControl returns promptly — the
// real outcome is preserved, not overridden to InterruptError.
func TestRunDeadlineWatchdog_FastInterruptStillWins(t *testing.T) {
	// NOT t.Parallel() — mutates package-level watchdogInterruptTimeoutAtomic.
	setWatchdogInterruptTimeoutForTest(t, 200*time.Millisecond)

	ci := &countingInterrupter{outcome: InterruptUnsupported}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	chUnsup, _ := runDeadlineWatchdog(ctx, ci)
	abort := <-chUnsup
	if !abort.fired {
		t.Fatal("abort.fired = false; want true on DeadlineExceeded")
	}
	if abort.outcome != InterruptUnsupported {
		t.Fatalf("abort.outcome = %v, want InterruptUnsupported (fast return preserves real outcome)", abort.outcome)
	}
}
