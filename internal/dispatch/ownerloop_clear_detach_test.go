package dispatch

// #2262: a turn's reaction clear must detach from the (possibly already
// cancelled) turn ctx via context.WithoutCancel — otherwise a
// shutdown-during-turn race makes the child WithTimeout born cancelled, every
// RemoveReaction short-circuits, and the ⏳ HOURGLASS hangs until the
// platform's ~12h reaction TTL.

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// TestDrainTurn_ClearsReactionsUnderCancelledCtx drives a drain turn whose
// ctx is cancelled while it runs: its delivery must still clear the batch's
// ⏳.
func TestDrainTurn_ClearsReactionsUnderCancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turns := 0
	var d *Dispatcher
	sender := &testSender{}
	sender.getOrCreate = func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
		turns++
		if turns == 1 {
			queueIM(t, d, "m1", "m2")
		} else {
			cancel() // shutdown lands mid drain turn
		}
		return fakeSession{}, 0, nil
	}
	d, rp := newReactorDispatcher(t, turn.QueueOptions{MaxDepth: 8}, sender)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runIMTurn(ctx, d, reactorKey, "owner", reactorMsg("m0", "owner"), true)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owner loop did not return")
	}
	wantRemoved(t, rp, "m0", "m1", "m2")
}
