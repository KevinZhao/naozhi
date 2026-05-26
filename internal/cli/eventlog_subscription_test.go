package cli

// R246-ARCH-12 / #792 (P0 subset): SubscribeNew returns a typed
// EventSubscription value that bundles the notify channel + cancel
// callback, hiding (*subscriber).closeOnce semantics from cross-package
// callers. These tests pin the four observable behaviours of the typed
// API so future tweaks cannot silently regress one of them.

import (
	"testing"
	"time"
)

func TestEventSubscription_NotifyFiresOnAppend(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	sub := l.SubscribeNew()
	defer sub.Cancel()

	l.Append(EventEntry{Type: "user", Summary: "hi"})

	select {
	case <-sub.Notify():
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected notify after Append, got timeout")
	}
}

func TestEventSubscription_CancelClosesNotify(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	sub := l.SubscribeNew()

	sub.Cancel()

	// Notify channel must be closed (not just empty) so a parked select
	// arm wakes up. Receiving from a closed channel is the only way to
	// observe "subscription torn down" without polling.
	select {
	case _, ok := <-sub.Notify():
		if ok {
			t.Fatalf("Notify channel delivered a real value after Cancel; want closed")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Notify channel still open 500ms after Cancel")
	}
}

func TestEventSubscription_CancelIdempotent(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	sub := l.SubscribeNew()

	sub.Cancel()
	// Second Cancel must be a no-op (no panic from double-close inside
	// closeOnce). This is what makes EventSubscription safe to drop into
	// defer chains alongside an explicit Cancel on the happy path.
	sub.Cancel()
}

func TestEventSubscription_CloseSubscribersWakesAll(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	subA := l.SubscribeNew()
	subB := l.SubscribeNew()

	l.CloseSubscribers()

	for i, s := range []EventSubscription{subA, subB} {
		select {
		case _, ok := <-s.Notify():
			if ok {
				t.Fatalf("sub[%d] Notify delivered a value, want closed", i)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("sub[%d] Notify not closed after CloseSubscribers", i)
		}
	}

	// Cancel after CloseSubscribers must remain a no-op (closeOnce guard).
	subA.Cancel()
	subB.Cancel()
}

func TestEventSubscription_SubscribeAfterClose(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.CloseSubscribers()

	// Subscribe-after-close must return a pre-closed channel so the
	// caller's select arm fires immediately instead of parking forever.
	// EventSubscription preserves that invariant from the underlying
	// Subscribe() path.
	sub := l.SubscribeNew()
	select {
	case _, ok := <-sub.Notify():
		if ok {
			t.Fatalf("Notify delivered a value after Subscribe-on-closed log")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Notify not pre-closed after Subscribe on already-closed log")
	}
	// Cancel still no-ops.
	sub.Cancel()
}
