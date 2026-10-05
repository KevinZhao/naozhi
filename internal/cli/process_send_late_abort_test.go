package cli

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

type lateAbortSendOut struct {
	res *clievent.SendResult
	err error
}

// startLateAbortSend runs a legacy Send in the background and waits for its
// stdin write, so the caller knows the Send has claimed the process.
func startLateAbortSend(t *testing.T, sh *passthroughShim, ctx context.Context, text string) <-chan lateAbortSendOut {
	t.Helper()
	out := make(chan lateAbortSendOut, 1)
	go func() {
		res, err := sh.proc.Send(ctx, text, nil, nil)
		out <- lateAbortSendOut{res, err}
	}()
	sh.expectWrite(t, 3*time.Second)
	return out
}

func awaitLateAbortSend(t *testing.T, out <-chan lateAbortSendOut, what string) lateAbortSendOut {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: Send still blocked", what)
		return lateAbortSendOut{}
	}
}

// abandonInterruptedSend starts Send A, interrupts it via control_request and
// cancels its context before the CLI answers the interrupt, as the cron
// deadline watchdog does.
func abandonInterruptedSend(t *testing.T, sh *passthroughShim) {
	t.Helper()
	ctxA, cancelA := context.WithCancel(context.Background())
	outA := startLateAbortSend(t, sh, ctxA, "A")
	emitInitAndWait(t, sh)
	if err := sh.proc.InterruptViaControl(); err != nil {
		t.Fatalf("InterruptViaControl: %v", err)
	}
	if in := sh.expectWrite(t, 2*time.Second); in.Type != "control_request" {
		t.Fatalf("stdin write = %q, want the control_request", in.Type)
	}
	cancelA()
	if o := awaitLateAbortSend(t, outA, "A"); o.err == nil {
		t.Fatalf("Send A returned %+v, want its context error", o.res)
	}
}

// emitInitAndWait waits until the running Send has read the init, so a
// canceled Send cannot leave it for readLoop to start an unowned turn with.
func emitInitAndWait(t *testing.T, sh *passthroughShim) {
	t.Helper()
	sh.emitInit("s1")
	testhelper.Eventually(t, func() bool { return sh.proc.SessionID() == "s1" }, 2*time.Second, "init read by Send")
}

func newLateAbortShim(t *testing.T) (*passthroughShim, chan clievent.SendResult) {
	t.Helper()
	sh := newPassthroughShim(t)
	sh.proc.turn.state = StateReady
	booked := make(chan clievent.SendResult, 8)
	sh.proc.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })
	t.Cleanup(sh.close)
	go sh.proc.readLoop()
	return sh, booked
}

// The aborted result of a Send that gave up arrives after the next Send
// claimed the process and its settle window passed. It belongs to the
// abandoned turn: booked, and never handed to the next Send as its answer.
func TestLegacySend_LateAbortedResultOfAbandonedSendIsNotNextAnswer(t *testing.T) {
	sh, booked := newLateAbortShim(t)
	abandonInterruptedSend(t, sh)

	outB := startLateAbortSend(t, sh, context.Background(), "B")
	sh.srv.SendStdout(abortedResult)
	sh.emitResult("s1", "B-answer")

	o := awaitLateAbortSend(t, outB, "B")
	if o.err != nil {
		t.Fatalf("Send B: %v", o.err)
	}
	if r := o.res; r.Text != "B-answer" || r.Aborted || r.SubType != "success" {
		t.Errorf("Send B = text %q, Aborted %v, SubType %q; want B-answer, false, success",
			r.Text, r.Aborted, r.SubType)
	}
	select {
	case r := <-booked:
		if !r.Aborted || r.SubType != "error_during_execution" {
			t.Errorf("booked = Aborted %v, SubType %q; want the abandoned turn's aborted result", r.Aborted, r.SubType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned turn's aborted result was not booked")
	}
}

// When the abandoned turn's own result was already read before the abort was
// armed, the abort produces no second result: the next Send's drain books
// that result and the next Send takes the result that follows.
func TestLegacySend_DrainedResultReleasesAbandonedAbort(t *testing.T) {
	sh, _ := newLateAbortShim(t)
	ctxA, cancelA := context.WithCancel(context.Background())
	outA := startLateAbortSend(t, sh, ctxA, "A")
	emitInitAndWait(t, sh)
	cancelA()
	if o := awaitLateAbortSend(t, outA, "A"); o.err == nil {
		t.Fatalf("Send A returned %+v, want its context error", o.res)
	}
	sh.emitResult("s1", "A-answer")
	testhelper.Eventually(t, func() bool { return len(sh.proc.eventCh) == 1 }, 2*time.Second, "A's result queued")
	// The interrupt that raced A's result: armed after the result was read.
	sh.proc.turn.abortRequested.arm()

	outB := startLateAbortSend(t, sh, context.Background(), "B")
	sh.emitResult("s1", "B-answer")

	o := awaitLateAbortSend(t, outB, "B")
	if o.err != nil {
		t.Fatalf("Send B: %v", o.err)
	}
	if o.res.Text != "B-answer" {
		t.Errorf("Send B text = %q, want B-answer", o.res.Text)
	}
}

// A Send interrupted while an abandoned turn's abort is still outstanding
// owns the next aborted result: the CLI may answer both aborts with one.
func TestLegacySend_InterruptedNextSendOwnsTheAbortedResult(t *testing.T) {
	sh, _ := newLateAbortShim(t)
	abandonInterruptedSend(t, sh)

	outB := startLateAbortSend(t, sh, context.Background(), "B")
	if err := sh.proc.InterruptViaControl(); err != nil {
		t.Fatalf("InterruptViaControl: %v", err)
	}
	sh.srv.SendStdout(abortedResult)

	o := awaitLateAbortSend(t, outB, "B")
	if o.err != nil {
		t.Fatalf("Send B: %v", o.err)
	}
	if !o.res.Aborted || o.res.SubType != "error_during_execution" {
		t.Errorf("Send B = Aborted %v, SubType %q; want its own abort's result", o.res.Aborted, o.res.SubType)
	}
}

// An abort that lands after a Send already read its result has no result of
// its own to come; the next Send still takes the result that follows, since
// only a Send that gave up leaves an abort for a late result.
func TestLegacySend_AbortAfterCompletedSendDoesNotHideNextAnswer(t *testing.T) {
	sh, _ := newLateAbortShim(t)
	outA := startLateAbortSend(t, sh, context.Background(), "A")
	sh.emitResult("s1", "A-answer")
	if o := awaitLateAbortSend(t, outA, "A"); o.err != nil || o.res.Text != "A-answer" {
		t.Fatalf("Send A = %+v, %v; want A-answer", o.res, o.err)
	}
	// The interrupt that raced A's result: armed after the result was read.
	sh.proc.turn.abortRequested.arm()

	outB := startLateAbortSend(t, sh, context.Background(), "B")
	sh.emitResult("s1", "B-answer")

	o := awaitLateAbortSend(t, outB, "B")
	if o.err != nil {
		t.Fatalf("Send B: %v", o.err)
	}
	if o.res.Text != "B-answer" {
		t.Errorf("Send B text = %q, want B-answer", o.res.Text)
	}
}
