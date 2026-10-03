package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/turn"
)

// #2185: /new must discard the queue (the #2013 drain-and-clear-reactions
// path) BEFORE the session reset. The reset synchronously fires the
// observer's KeyRetired → Orchestrator.Cleanup, which deletes the queue ring
// without surfacing the parked messages' HOURGLASS reactions. Turns.Reset
// owns that order now; this test models the reset's side effect with
// Cleanup in the Sender, exactly what production's KeyRetired closure calls.
// Why the order matters is internal/turn's
// TestQueue_CleanupLeavesNothingToDiscard.

// TestNewOrder_DiscardBeforeReset_ClearsReactions: the drain+clear sees the
// populated ring, so the parked reactions are cleared, and the reset then
// leaves the key idle.
func TestNewOrder_DiscardBeforeReset_ClearsReactions(t *testing.T) {
	resets := 0
	var d *Dispatcher
	d, rp := newReactorDispatcher(t, turn.QueueOptions{MaxDepth: 8}, &testSender{
		reset: func(key string, _ bool) {
			resets++
			d.turns.(*turn.Orchestrator).Cleanup(key) // models router.Reset → the observer's KeyRetired → Cleanup
		},
	})
	holdKey(t, d, reactorKey)
	queueIM(t, d, "m1", "m2")

	d.BuildHandler()(context.Background(), reactorMsg("m3", "/new"))

	if resets != 1 {
		t.Fatalf("session resets = %d, want 1", resets)
	}
	wantRemoved(t, rp, "m1", "m2")
	// The teardown left the key idle: the next message becomes its owner.
	holdKey(t, d, reactorKey)
}
