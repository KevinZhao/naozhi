package cron

import (
	"context"
	"errors"
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

	result, abort, err := sendWithWatchdog(sendCtx, sendCancel, sess, "hi")
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

	_, abort, err := sendWithWatchdog(sendCtx, sendCancel, sess, "hi")
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
