package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDetachedNotifyCtx_DetachesFromParent locks the core property:
// detachedNotifyCtx ignores its caller's ctx tree and returns a fresh
// Background-rooted ctx that survives parent cancellation. This is the
// guarantee #632 (R247-ARCH-10) needs callers to depend on so that
// "user-visible reply on shutdown / panic" cannot silently regress.
func TestDetachedNotifyCtx_DetachesFromParent(t *testing.T) {
	t.Parallel()
	// The factory takes only timeout — no parent ctx, by design — so the
	// "no inheritance" property is structural rather than a runtime test.
	// The behavioural check below confirms the timer fires on its own
	// budget regardless of caller-side cancellation.
	notifyCtx, cancel := detachedNotifyCtx(50 * time.Millisecond)
	defer cancel()

	// Cancelling some unrelated ctx must not cancel notifyCtx.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	parentCancel()
	_ = parentCtx

	select {
	case <-notifyCtx.Done():
		t.Fatalf("notifyCtx cancelled before its own deadline; "+
			"expected detached lifecycle, got Err=%v", notifyCtx.Err())
	case <-time.After(10 * time.Millisecond):
		// expected: notifyCtx still alive on its own clock.
	}

	// And the deadline does eventually fire.
	select {
	case <-notifyCtx.Done():
		if !errors.Is(notifyCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", notifyCtx.Err())
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("notifyCtx did not fire within deadline")
	}
}

// TestDetachedNotifyCtx_ManualCancelReleases pins that the returned cancel
// closes notifyCtx promptly so callers that defer cancel() don't leak the
// timer goroutine on the happy path.
func TestDetachedNotifyCtx_ManualCancelReleases(t *testing.T) {
	t.Parallel()
	notifyCtx, cancel := detachedNotifyCtx(time.Hour)
	cancel()
	select {
	case <-notifyCtx.Done():
		if !errors.Is(notifyCtx.Err(), context.Canceled) {
			t.Fatalf("expected Canceled after manual cancel, got %v", notifyCtx.Err())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("notifyCtx not closed by manual cancel")
	}
}
