package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/turn"
)

// #2185: /new must discard the queue (the #2013 drain-and-clear-reactions
// path) BEFORE the session reset. The reset synchronously fires the
// observer's KeyRetired → msgQueue.Cleanup, which deletes the queue ring
// without surfacing the parked messages' HOURGLASS reactions. Turns.Reset
// owns that order now; these tests model the reset's side effect with
// q.Cleanup in the Sender, exactly what production's KeyRetired closure
// calls.

// TestNewOrder_DiscardBeforeReset_ClearsReactions: the drain+clear sees the
// populated ring, so the parked reactions are cleared, and the reset then
// leaves the key idle with its queue entry gone.
func TestNewOrder_DiscardBeforeReset_ClearsReactions(t *testing.T) {
	q := turn.NewQueueWithMode(8, 0, turn.ModeCollect)
	resets := 0
	d, rp := newReactorDispatcher(t, q, &testSender{
		reset: func(key string, _ bool) {
			resets++
			q.Cleanup(key) // models router.Reset → the observer's KeyRetired → Cleanup
		},
	})
	q.Enqueue(reactorKey, turn.Msg{Text: "owner"})
	queueIM(d, q, "m1", "m2")

	d.BuildHandler()(context.Background(), reactorMsg("m3", "/new"))

	if resets != 1 {
		t.Fatalf("session resets = %d, want 1", resets)
	}
	wantRemoved(t, rp, "m1", "m2")
	// The teardown left the key idle: the next Enqueue becomes owner. That
	// alone is the discard's doing (it clears busy); gen 0 is what pins
	// Cleanup, because the discard bumped the retained entry's gen to 1 and
	// only deleting the entry resets it (cf. TestQueue_Cleanup_RemovesMapEntry).
	isOwner, _, _, gen, _ := q.Enqueue(reactorKey, turn.Msg{Text: "post-teardown"})
	if !isOwner {
		t.Error("queue not idle after teardown: Enqueue did not become owner")
	}
	if gen != 0 {
		t.Errorf("Cleanup did not delete the map entry: next Enqueue got gen %d, want 0", gen)
	}
}

// TestNewOrder_ResetBeforeDiscard_LeavesReactions documents why the order
// matters: once Cleanup (the reset's side effect) has run, the ring is gone
// and a discard has nothing left to report, so the HOURGLASS marks would
// hang.
func TestNewOrder_ResetBeforeDiscard_LeavesReactions(t *testing.T) {
	q := turn.NewQueueWithMode(8, 0, turn.ModeCollect)
	q.Enqueue(reactorKey, turn.Msg{Text: "owner"})
	q.Enqueue(reactorKey, turn.Msg{Text: "f1", MessageID: "m1"})
	q.Cleanup(reactorKey)
	if dropped := q.DiscardAndReturn(reactorKey); dropped != nil {
		t.Fatalf("discard after Cleanup reported %+v, want nothing (ring already gone)", dropped)
	}
}
