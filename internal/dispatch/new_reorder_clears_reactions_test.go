package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// #2185: the /new command handler must run discardQueue (the #2013
// drain-and-clear-reactions path) BEFORE router.Reset. Reset synchronously
// fires onKeyRetired → msgQueue.Cleanup, which deletes the queue ring without
// surfacing the parked messages' HOURGLASS reactions. If discardQueue runs
// AFTER Reset, the ring is already empty and the ⏳ marks hang until the
// platform reaction-cache TTL (feishu: 12h).
//
// These tests model Reset's synchronous side effect with msgQueue.Cleanup
// (exactly what the production onKeyRetired closure calls, server.go:510) and
// pin both orderings so a future refactor cannot silently regress the order.

// TestNewOrder_DiscardBeforeReset_ClearsReactions is the fixed ordering: the
// drain+clear sees the populated ring, so the parked reactions are cleared.
func TestNewOrder_DiscardBeforeReset_ClearsReactions(t *testing.T) {
	d, rp := newReactorDispatcher(t)
	const key = "im:direct:u1:general"

	d.queue.Enqueue(key, QueuedMsg{Text: "owner", MessageID: "m0"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f1", MessageID: "m1"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f2", MessageID: "m2"})

	msg := platform.IncomingMessage{Platform: "fake", ChatID: "u1"}

	// Fixed order: discardQueue first (while ring is populated), then the
	// Reset-equivalent Cleanup.
	d.discardQueue(context.Background(), msg, key)
	d.queue.Cleanup(key) // models router.Reset → onKeyRetired → Cleanup

	got := removedIDs(rp)
	want := map[string]bool{"m1": true, "m2": true}
	if len(got) != 2 {
		t.Fatalf("expected 2 reactions cleared (m1, m2), got %v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected reaction cleared: %q", id)
		}
	}
	if d.queue.Depth(key) != 0 {
		t.Errorf("queue depth = %d after teardown, want 0", d.queue.Depth(key))
	}
}

// TestNewOrder_ResetBeforeDiscard_LeavesReactions documents the regression the
// fix prevents: when Cleanup (Reset's side effect) runs first, the ring is
// emptied and discardQueue has nothing left to clear — the HOURGLASS marks
// would hang. This pins WHY the order matters; if someone reverts the handler
// to Reset-before-discard this asserts the (bad) old behaviour, making the
// coupling explicit.
func TestNewOrder_ResetBeforeDiscard_LeavesReactions(t *testing.T) {
	d, rp := newReactorDispatcher(t)
	const key = "im:direct:u1:general"

	d.queue.Enqueue(key, QueuedMsg{Text: "owner", MessageID: "m0"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f1", MessageID: "m1"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f2", MessageID: "m2"})

	msg := platform.IncomingMessage{Platform: "fake", ChatID: "u1"}

	// Buggy order: Cleanup first empties the ring, so discardQueue's
	// DiscardAndReturn returns nil and no reaction is cleared.
	d.queue.Cleanup(key)
	d.discardQueue(context.Background(), msg, key)

	if got := removedIDs(rp); len(got) != 0 {
		t.Fatalf("Reset-before-discard should clear nothing (ring already gone), got %v", got)
	}
}
