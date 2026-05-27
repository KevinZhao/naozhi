package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestResolveReplyCtx_HappyPathNoSwap pins the cheap branch: a live ctx
// (no error) is returned unchanged with a nil cleanup so callers using
// `if cleanup != nil { defer cleanup() }` skip the defer entirely.
func TestResolveReplyCtx_HappyPathNoSwap(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	got, cleanup := resolveReplyCtx(parent)
	if got != parent {
		t.Fatalf("happy path returned a different ctx (%v) — must pass through unchanged", got)
	}
	if cleanup != nil {
		t.Fatalf("happy path cleanup non-nil — caller skips defer when no swap happens")
	}
}

// TestResolveReplyCtx_SwapsOnCanceled covers the shutdown-style branch:
// a canceled parent ctx must be replaced with a fresh ctx that has a
// real deadline (the shutdown reply budget). Without this swap the
// platform.Reply call inside the error path silently drops because the
// outer ctx is already Done. R242-GO-4 / #550.
func TestResolveReplyCtx_SwapsOnCanceled(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	cancel() // simulate shutdown / SIGTERM

	got, cleanup := resolveReplyCtx(parent)
	if got == parent {
		t.Fatal("canceled parent must be swapped — returned ctx is the same instance")
	}
	if cleanup == nil {
		t.Fatal("swap branch must return a non-nil cleanup")
	}
	defer cleanup()
	if got.Err() != nil {
		t.Fatalf("swapped ctx already errored: %v", got.Err())
	}
	dl, ok := got.Deadline()
	if !ok {
		t.Fatal("swapped ctx has no deadline — NotifyCtx must bound it")
	}
	if d := time.Until(dl); d <= 0 || d > shutdownReplyTimeout+time.Second {
		t.Fatalf("swapped ctx deadline %v out of expected range (0, %v+1s]", d, shutdownReplyTimeout)
	}
}

// TestResolveReplyCtx_DeadlineExceededNotSwapped — only context.Canceled
// triggers the swap. A turn that legitimately ran out of its configured
// per-turn budget (DeadlineExceeded) keeps the original ctx so we don't
// silently lengthen a budget the operator set.
func TestResolveReplyCtx_DeadlineExceededNotSwapped(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if !errors.Is(parent.Err(), context.DeadlineExceeded) {
		t.Skipf("setup precondition failed: parent.Err = %v", parent.Err())
	}

	got, cleanup := resolveReplyCtx(parent)
	if got != parent {
		t.Fatal("DeadlineExceeded must NOT trigger swap — returned ctx differs from input")
	}
	if cleanup != nil {
		t.Fatal("DeadlineExceeded path must skip cleanup — got non-nil")
	}
}

// TestResolveReplyCtx_NilParent — defensive: a nil ctx (some callers
// pass nil for "no parent") yields a fresh shutdown-budget ctx + cleanup.
func TestResolveReplyCtx_NilParent(t *testing.T) {
	t.Parallel()
	got, cleanup := resolveReplyCtx(nil)
	if got == nil {
		t.Fatal("nil parent: returned ctx is nil")
	}
	if cleanup == nil {
		t.Fatal("nil parent: cleanup is nil — must always return cleanup on swap")
	}
	defer cleanup()
	if got.Err() != nil {
		t.Fatalf("nil parent: returned ctx already errored: %v", got.Err())
	}
}
