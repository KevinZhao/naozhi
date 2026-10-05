package cron

// R242-GO-13 regression: deliverNotice must dispatch the IM reply on a
// goroutine tracked by triggerWG, so the cron-tick callback (or
// freshContextPreflightP0 error path) returns immediately and the next
// tick / preflight is not blocked by the platform reply chain.
//
// Without the async wrapper, finishRun's recordResult landed first but
// the calling goroutine still spent up to cronNotifyTimeout (30s) on the
// IM webhook before the cron lib could observe the tick as done.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestDeliverNotice_NoTargetIsNoOp pins the contract that an unset target
// short-circuits before the goroutine spawn — otherwise every "no notify
// configured" job would still leak a triggerWG.Add+Done pair on every tick.
func TestDeliverNotice_NoTargetIsNoOp(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	s.deliverNotice(NotifyTarget{}, "ignored")
	// triggerWG.Wait must return immediately; if Add(1) had run we would
	// hang here (no Done was scheduled).
	done := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("triggerWG.Wait blocked on empty target — unset NotifyTarget must short-circuit before Add(1)")
	}
}

// TestDeliverNotice_EmptyTextIsNoOp pins the contract that an empty text
// payload short-circuits before the goroutine spawn. R20260526-CR-017:
// without this guard, an empty-result run would still spawn a goroutine
// running platform.SplitText("", maxLen) → [""] and burn one
// limits.PlatformReplyMaxAttempts retry on a zero-byte chunk. Mirror shape of
// TestDeliverNotice_NoTargetIsNoOp — verify triggerWG.Wait returns
// immediately, proving Add(1) never ran.
func TestDeliverNotice_EmptyTextIsNoOp(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	target := NotifyTarget{Platform: "feishu", ChatID: "oc_x"}
	s.deliverNotice(target, "")
	done := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("triggerWG.Wait blocked on empty text — empty payload must short-circuit before Add(1)")
	}
}

// TestDeliverNotice_SetTargetTracksWG is the core async-dispatch pin:
// after deliverNotice returns the goroutine must already be Add'd onto
// triggerWG, and Wait must drain it. No NotifySender is configured so
// notifyTarget short-circuits at the `sender == nil` check — we are testing
// the wrapper, not the IM transport.
func TestDeliverNotice_SetTargetTracksWG(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	target := NotifyTarget{Platform: "feishu", ChatID: "oc_x"}
	s.deliverNotice(target, "irrelevant")
	// triggerWG.Wait must complete: the goroutine notifyTarget body sees
	// nil platform and returns immediately, so Done fires fast.
	done := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("triggerWG.Wait did not drain after deliverNotice — async wrapper not Done'ing")
	}
}

// blockingReplier is a PlatformReplier whose Reply signals entered and then
// holds until release is closed, so a test can observe deliverNotice's return
// while delivery is provably still in flight.
type blockingReplier struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (r *blockingReplier) MaxReplyLength() int                    { return 4096 }
func (r *blockingReplier) Split(text string, maxLen int) []string { return []string{text} }
func (r *blockingReplier) UsesSingleUseReplyToken() bool          { return false }

func (r *blockingReplier) Reply(ctx context.Context, chatID, text string) (string, error) {
	if r.calls.Add(1) == 1 {
		close(r.entered)
	}
	select {
	case <-r.release:
		return "", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type blockingSender struct{ r *blockingReplier }

func (b blockingSender) Lookup(platform string) (PlatformReplier, bool) {
	if platform != "fake" {
		return nil, false
	}
	return b.r, true
}

// TestDeliverNotice_ReturnsBeforeNotifyTarget verifies the call site is not
// blocked by the IM transport: deliverNotice must return while Reply is still
// held open. A synchronous regression cannot return before release is closed,
// so the test fails regardless of machine load.
func TestDeliverNotice_ReturnsBeforeNotifyTarget(t *testing.T) {
	t.Parallel()
	r := &blockingReplier{entered: make(chan struct{}), release: make(chan struct{})}
	s := &Scheduler{}
	s.configMapsPtr.Store(&cronConfigMaps{notifySender: blockingSender{r: r}})

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		s.deliverNotice(NotifyTarget{Platform: "fake", ChatID: "c"}, "x")
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		close(r.release)
		t.Fatal("deliverNotice blocked while Reply was held; the synchronous notifyTarget path is back — R242-GO-13 regressed")
	}
	select {
	case <-r.entered:
	case <-time.After(5 * time.Second):
		close(r.release)
		t.Fatal("delivery goroutine never reached Reply")
	}
	close(r.release)

	drained := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("triggerWG.Wait did not drain after Reply was released")
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("Reply called %d times, want 1", n)
	}
}

// TestDeliverNotice_BurstAddsCompleteSerially verifies the async wrapper still
// honours the Stop() drain contract: many serial deliverNotice calls must all
// be Add'd onto triggerWG and observed by Wait. Calls are issued on the
// caller goroutine (deliverNotice does its own Add(1) BEFORE `go`) so we
// avoid racing the test goroutines against triggerWG.Wait — that race is what
// sync.WaitGroup explicitly disallows (Add concurrent with Wait).
func TestDeliverNotice_BurstAddsCompleteSerially(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	const n = 32
	target := NotifyTarget{Platform: "no-such-plat", ChatID: "y"}
	for i := 0; i < n; i++ {
		s.deliverNotice(target, "burst")
	}
	done := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("triggerWG.Wait did not drain bursty deliverNotice — R242-GO-13 wrapper broken")
	}
}
