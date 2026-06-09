package cron

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingInterrupter is a deadlineInterrupter test stub that records how
// many times InterruptViaControl was called and what outcome to return.
// Mirrors the minimal surface area runDeadlineWatchdog actually needs —
// nothing else from cron.Session (such as Send) is exercised by the
// watchdog, so this stub deliberately stays narrower than Session.
type countingInterrupter struct {
	calls   atomic.Int32
	outcome InterruptOutcome
}

func (c *countingInterrupter) InterruptViaControl() InterruptOutcome {
	c.calls.Add(1)
	return c.outcome
}

// TestRunDeadlineWatchdog_FiresOnDeadlineExceeded asserts the watchdog
// invokes InterruptViaControl exactly once when ctx ends with
// DeadlineExceeded. This is the regression test for the bug the reviewer
// caught: pre-fix, the cron's deadline path called sess.InterruptViaControl
// AFTER sess.Send returned, by which time Process.State had already
// transitioned to Ready and the call was a silent no-op. Now the watchdog
// runs concurrently and fires while the process is still Running.
func TestRunDeadlineWatchdog_FiresOnDeadlineExceeded(t *testing.T) {
	t.Parallel()
	ci := &countingInterrupter{outcome: InterruptSent}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	ch, _ := runDeadlineWatchdog(ctx, ci, watchdogInterruptTimeoutDefault)
	abort := <-ch
	if !abort.fired {
		t.Fatalf("abort.fired = false, want true on DeadlineExceeded")
	}
	if abort.outcome != InterruptSent {
		t.Fatalf("abort.outcome = %v, want InterruptSent", abort.outcome)
	}
	if got := ci.calls.Load(); got != 1 {
		t.Fatalf("InterruptViaControl call count = %d, want 1", got)
	}
}

// TestRunDeadlineWatchdog_SkipsOnExplicitCancel proves the watchdog does
// NOT fire InterruptViaControl when ctx ends because the caller cancelled
// explicitly (the success / non-deadline error path of cron.executeOpt).
// Calling Interrupt on a successful turn would set spurious settle-flags
// and force the next Send into a 500ms drain loop for nothing.
func TestRunDeadlineWatchdog_SkipsOnExplicitCancel(t *testing.T) {
	t.Parallel()
	ci := &countingInterrupter{outcome: InterruptSent}
	ctx, cancel := context.WithCancel(context.Background())

	ch, _ := runDeadlineWatchdog(ctx, ci, watchdogInterruptTimeoutDefault)
	cancel()
	abort := <-ch
	if abort.fired {
		t.Fatalf("abort.fired = true on explicit cancel; want false (only DeadlineExceeded should fire)")
	}
	if got := ci.calls.Load(); got != 0 {
		t.Fatalf("InterruptViaControl call count = %d, want 0 on explicit cancel", got)
	}
}

// TestRunDeadlineWatchdog_NilGuard verifies the R249-GO-3 defensive guard:
// nil ctx or nil sess returns a pre-completed channel with a zero
// abortResult instead of panicking inside the goroutine. Production never
// passes nil, but the guard keeps a caller bug from corrupting the run
// goroutine via robfig/cron's recover chain.
func TestRunDeadlineWatchdog_NilGuard(t *testing.T) {
	t.Parallel()
	t.Run("nil ctx", func(t *testing.T) {
		t.Parallel()
		ci := &countingInterrupter{outcome: InterruptSent}
		ch, stop := runDeadlineWatchdog(nil, ci, watchdogInterruptTimeoutDefault) //nolint:staticcheck // intentional nil for guard test
		if stop() {
			t.Fatalf("nil-guard stop() = true; want false (result already on channel)")
		}
		abort := <-ch
		if abort.fired {
			t.Fatalf("abort.fired = true with nil ctx; want false")
		}
		if got := ci.calls.Load(); got != 0 {
			t.Fatalf("InterruptViaControl call count = %d, want 0", got)
		}
	})
	t.Run("nil sess", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, _ := runDeadlineWatchdog(ctx, nil, watchdogInterruptTimeoutDefault)
		abort := <-ch
		if abort.fired {
			t.Fatalf("abort.fired = true with nil sess; want false")
		}
	})
}

// TestRunDeadlineWatchdog_PropagatesUnsupportedOutcome covers the ACP
// backend case: when InterruptViaControl reports the protocol can't
// abort a turn, the watchdog must still mark fired=true and pass the
// outcome through so the caller's slog line distinguishes "interrupt
// fired but backend doesn't support it" from "no interrupt attempted".
func TestRunDeadlineWatchdog_PropagatesUnsupportedOutcome(t *testing.T) {
	t.Parallel()
	ci := &countingInterrupter{outcome: InterruptUnsupported}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	ch, _ := runDeadlineWatchdog(ctx, ci, watchdogInterruptTimeoutDefault)
	abort := <-ch
	if !abort.fired {
		t.Fatal("abort.fired = false; ACP path should still record an attempt")
	}
	if abort.outcome != InterruptUnsupported {
		t.Fatalf("abort.outcome = %v, want InterruptUnsupported", abort.outcome)
	}
}
