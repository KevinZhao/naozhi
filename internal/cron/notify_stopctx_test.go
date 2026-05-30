package cron

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// fakeBlockingPlatform is a platform.Platform whose Reply blocks until ctx
// is cancelled. Used by R243-SEC-14 (#799) to verify cron notifyTarget's
// replyCtx propagates s.stopCtx cancellation into a hung webhook.
type fakeBlockingPlatform struct {
	maxLen     int
	mu         sync.Mutex
	seenCancel error // ctx.Err observed at unblock
	released   chan struct{}
}

func newFakeBlockingPlatform(maxLen int) *fakeBlockingPlatform {
	return &fakeBlockingPlatform{maxLen: maxLen, released: make(chan struct{})}
}

func (f *fakeBlockingPlatform) Name() string { return "fake-block" }
func (f *fakeBlockingPlatform) RegisterRoutes(*http.ServeMux, platform.MessageHandler) {
}
func (f *fakeBlockingPlatform) Reply(ctx context.Context, _ platform.OutgoingMessage) (string, error) {
	<-ctx.Done()
	f.mu.Lock()
	if f.seenCancel == nil {
		f.seenCancel = ctx.Err()
		close(f.released)
	}
	f.mu.Unlock()
	return "", ctx.Err()
}
func (f *fakeBlockingPlatform) EditMessage(context.Context, string, string) error { return nil }
func (f *fakeBlockingPlatform) MaxReplyLength() int                               { return f.maxLen }
func (f *fakeBlockingPlatform) cancelObserved() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seenCancel
}

// TestR243SEC14_NotifyTargetCancelsOnStopCtx pins #799: notifyTarget's
// replyCtx must chain to s.stopCtx so a hung webhook unblocks the moment
// Stop fires, instead of waiting for the per-target cronNotifyTimeout
// (30s). Pre-fix the parent was context.Background, which meant
// triggerWG.Wait stayed parked at the full stopBudget even after stopCtx
// had cancelled.
func TestR243SEC14_NotifyTargetCancelsOnStopCtx(t *testing.T) {
	t.Parallel()
	fp := newFakeBlockingPlatform(64)
	stopCtx, stopCancel := context.WithCancel(context.Background())
	s := &Scheduler{
		stopCtx: stopCtx,
	}
	s.configMapsPtr.Store(&cronConfigMaps{
		platforms: map[string]platform.Platform{"fake-block": fp},
	})

	done := make(chan struct{})
	go func() {
		// notifyTarget will block on Reply until stopCancel propagates.
		s.notifyTarget("fake-block", "chat-x", "hello")
		close(done)
	}()

	// Give Reply a moment to enter its <-ctx.Done() wait, then cancel
	// stopCtx. notifyTarget must return promptly — well under the 30s
	// per-target ceiling.
	time.Sleep(20 * time.Millisecond)
	stopCancel()

	select {
	case <-done:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("notifyTarget did not return within 2s after stopCtx cancel; " +
			"replyCtx is not parented on s.stopCtx (R243-SEC-14 / #799 regression)")
	}
	if got := fp.cancelObserved(); got != context.Canceled && got != context.DeadlineExceeded {
		t.Errorf("Reply ctx error = %v, want context.Canceled or DeadlineExceeded", got)
	}
}

// TestR243SEC14_NotifyTargetNilStopCtxFallback covers the defensive
// fallback: a hand-constructed *Scheduler (e.g. test fake) without
// stopCtx wired must still have its per-target timeout enforced. The
// fallback parent is context.Background; the cronNotifyTimeout ceiling
// continues to bound the call. We bound the assertion with a small
// override of the real timer would take 30s — instead we just confirm
// the call returns at all when no parent cancel is wired (i.e. doesn't
// nil-dereference s.stopCtx).
func TestR243SEC14_NotifyTargetNilStopCtxFallback(t *testing.T) {
	t.Parallel()
	// fakePartialPlatform's failAt=1000 makes every Reply succeed —
	// notifyTarget runs the full chunk loop and returns. The point of
	// this test is just "does not panic / nil-deref".
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 64}
	s := &Scheduler{
		// stopCtx intentionally unset
	}
	s.configMapsPtr.Store(&cronConfigMaps{
		platforms: map[string]platform.Platform{"fake-notify": fp},
	})
	s.notifyTarget("fake-notify", "chat-x", "hello world")
	if fp.uniqueChunks() == 0 {
		t.Errorf("notifyTarget with nil stopCtx should still send chunks; got 0")
	}
}
