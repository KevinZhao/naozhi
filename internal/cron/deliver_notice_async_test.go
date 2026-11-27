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

// TestDeliverNotice_EmptyTargetOrTextSendsNothing: a job with no notify target,
// or a run with an empty result, sends nothing — an empty text would otherwise
// burn a reply attempt on a zero-byte chunk.
func TestDeliverNotice_EmptyTargetOrTextSendsNothing(t *testing.T) {
	t.Parallel()
	r := &blockingReplier{entered: make(chan struct{}), release: make(chan struct{})}
	close(r.release)
	s := &Scheduler{}
	s.configMapsPtr.Store(&cronConfigMaps{notifySender: blockingSender{r: r}})

	s.deliverNotice(NotifyTarget{}, "ignored")
	s.deliverNotice(NotifyTarget{Platform: "fake", ChatID: "c"}, "")
	s.triggerWG.Wait()
	if n := r.calls.Load(); n != 0 {
		t.Fatalf("Reply called %d times, want 0", n)
	}
}
