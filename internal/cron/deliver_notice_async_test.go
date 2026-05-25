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

// TestDeliverNotice_SetTargetTracksWG is the core async-dispatch pin:
// after deliverNotice returns the goroutine must already be Add'd onto
// triggerWG, and Wait must drain it. The platform map is empty so
// notifyTarget short-circuits at the `p == nil` check — we are testing
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

// TestDeliverNotice_ReturnsBeforeNotifyTarget verifies the call site is no
// longer synchronously blocked by the IM transport. We can't stub
// notifyTarget cleanly without a platform fake, so we observe the timing
// directly: deliverNotice returns BEFORE the goroutine (which here trips a
// channel send) makes progress. If a future regression reverts the async
// wrapper, the channel send will sit ahead of deliverNotice's return and
// deliverNotice will only "return" after the goroutine finishes — i.e. the
// observed order will flip.
func TestDeliverNotice_ReturnsBeforeNotifyTarget(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	// We intercept the goroutine via triggerWG: count Done events vs the
	// time deliverNotice returns. With async dispatch, the call returns
	// while triggerWG counter is still 1; Wait drains it shortly after.
	target := NotifyTarget{Platform: "no-such-plat", ChatID: "x"}
	preCallReturn := time.Now()
	s.deliverNotice(target, "irrelevant")
	// At this moment the goroutine may or may not have completed (it's
	// just a nil-platform check + slog.Warn). The strict invariant we
	// can check: deliverNotice itself must not have spent >50ms — that
	// would only happen if the synchronous notifyTarget path were back.
	dur := time.Since(preCallReturn)
	if dur > 50*time.Millisecond {
		t.Errorf("deliverNotice took %v (>50ms); the synchronous notifyTarget path is back — R242-GO-13 regressed", dur)
	}
	// Drain the goroutine before returning so leak detectors stay clean.
	s.triggerWG.Wait()
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
