package cron

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSendSession satisfies cron.Session for sendWithWatchdog tests.
// The send func is configurable per-test so each scenario (fast
// success, deadline-exceeded, explicit cancel) controls Send's
// behaviour. interruptCalls counts InterruptViaControl invocations so
// tests assert the watchdog fired or not without inspecting goroutine
// state.
type fakeSendSession struct {
	send             func(ctx context.Context, text string) (SendResult, error)
	interruptCalls   atomic.Int32
	interruptOutcome InterruptOutcome
}

func (f *fakeSendSession) Send(ctx context.Context, text string) (SendResult, error) {
	return f.send(ctx, text)
}

func (f *fakeSendSession) SessionID() string { return "" }

func (f *fakeSendSession) InterruptViaControl() InterruptOutcome {
	f.interruptCalls.Add(1)
	return f.interruptOutcome
}

// TestSendWithWatchdog_SuccessNoFire asserts the happy path: Send
// returns quickly, sendCancel triggers the watchdog to exit via the
// ctx.Err()==Canceled branch (NOT DeadlineExceeded), abort.fired stays
// false, and InterruptViaControl is never called. R215-ARCH-P2-5 / #581
// — the post-refactor extraction must preserve this contract.
func TestSendWithWatchdog_SuccessNoFire(t *testing.T) {
	t.Parallel()

	sess := &fakeSendSession{
		send: func(ctx context.Context, text string) (SendResult, error) {
			return SendResult{Text: "ok", SessionID: "sid"}, nil
		},
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sendCancel()

	// R20260607-GO-4 (#1904): sendWithWatchdog is now a *Scheduler method.
	// A zero-value Scheduler reports a 0 timeout, which runDeadlineWatchdog
	// falls back to the default — fine here since the deadline is ctx-driven.
	s := &Scheduler{}
	result, abort, err := s.sendWithWatchdog(sendCtx, sendCancel, sess, "hi")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result.Text != "ok" {
		t.Fatalf("result.Text = %q, want %q", result.Text, "ok")
	}
	if abort.fired {
		t.Fatal("abort.fired = true on success path; want false (watchdog must not fire)")
	}
	if got := sess.interruptCalls.Load(); got != 0 {
		t.Fatalf("InterruptViaControl call count = %d, want 0", got)
	}
}

// TestSendWithWatchdog_DeadlineFiresInterrupt asserts that when Send
// blocks past sendCtx's deadline, the watchdog fires
// InterruptViaControl exactly once and returns abort.fired=true with
// the real outcome — and that the helper returns Send's error
// (DeadlineExceeded). Pins the ordering invariant the godoc calls out:
// drain abortCh AFTER sendCancel so the recorded outcome reflects the
// in-flight interrupt write.
func TestSendWithWatchdog_DeadlineFiresInterrupt(t *testing.T) {
	t.Parallel()

	sess := &fakeSendSession{
		send: func(ctx context.Context, text string) (SendResult, error) {
			<-ctx.Done()
			return SendResult{}, ctx.Err()
		},
		interruptOutcome: InterruptSent,
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer sendCancel()

	s := &Scheduler{}
	_, abort, err := s.sendWithWatchdog(sendCtx, sendCancel, sess, "hi")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if !abort.fired {
		t.Fatal("abort.fired = false on deadline path; want true")
	}
	if abort.outcome != InterruptSent {
		t.Fatalf("abort.outcome = %v, want InterruptSent", abort.outcome)
	}
	if got := sess.interruptCalls.Load(); got != 1 {
		t.Fatalf("InterruptViaControl call count = %d, want 1", got)
	}
}

// TestSendWithWatchdog_SuccessStopsCallback is the regression test for
// R20260603140013-GO-1 (#1705): on the success path stopWatchdog()
// deregisters the context.AfterFunc callback BEFORE sendCancel(), so the
// runtime never spawns the callback goroutine. Pre-fix every successful
// Send cancelled ctx with the callback still registered, forcing one
// wasted goroutine spawn + chan send. We assert no net goroutine growth
// across a burst of N successful sendWithWatchdog calls — the steady
// state must stay at the baseline, not baseline+N transient callbacks.
func TestSendWithWatchdog_SuccessStopsCallback(t *testing.T) {
	// Not Parallel: NumGoroutine() is process-global and a sibling
	// t.Parallel test spawning goroutines would skew the delta.
	const N = 200

	baseline := runtime.NumGoroutine()

	s := &Scheduler{}
	for i := 0; i < N; i++ {
		sess := &fakeSendSession{
			send: func(ctx context.Context, text string) (SendResult, error) {
				return SendResult{Text: "ok"}, nil
			},
		}
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, abort, err := s.sendWithWatchdog(sendCtx, sendCancel, sess, "hi")
		if err != nil {
			t.Fatalf("send %d: err = %v, want nil", i, err)
		}
		if abort.fired {
			t.Fatalf("send %d: abort.fired = true; want false", i)
		}
		if got := sess.interruptCalls.Load(); got != 0 {
			t.Fatalf("send %d: InterruptViaControl count = %d, want 0", i, got)
		}
	}

	// Let any goroutine the buggy path would have spawned get scheduled.
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)

	delta := runtime.NumGoroutine() - baseline
	// Allow a small slack for unrelated runtime/test-framework goroutines;
	// the bug would have left up to N transient callback goroutines, so any
	// threshold well below N catches it while tolerating noise.
	if delta > N/10 {
		t.Fatalf("goroutine delta = %d after %d successful sends; want ~0 (callback should be stopped, not spawned)", delta, N)
	}
}
