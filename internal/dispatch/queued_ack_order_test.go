package dispatch

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// slowAdd makes rp's add of msgID block until release closes, closing adding
// once it started; it releases itself after delay, long after a clear that
// does not wait for it would have run.
func slowAdd(rp *fakeReactorPlatform, msgID string, delay time.Duration) (adding <-chan struct{}) {
	started, release := make(chan struct{}), make(chan struct{})
	rp.beforeAdd = func(id string) {
		if id == msgID {
			close(started)
			<-release
		}
	}
	go func() {
		<-started
		time.Sleep(delay)
		close(release)
	}()
	return started
}

// wantAddBeforeRemove fails unless msgID's ⏳ was added once and then
// removed once, in that order.
func wantAddBeforeRemove(t *testing.T, rp *fakeReactorPlatform, msgID string) {
	t.Helper()
	rp.mu.Lock()
	defer rp.mu.Unlock()
	var got []string
	for _, e := range rp.seq {
		if e == "add:"+msgID || e == "remove:"+msgID {
			got = append(got, e)
		}
	}
	if want := []string{"add:" + msgID, "remove:" + msgID}; !slices.Equal(got, want) {
		t.Fatalf("%s reactions = %v, want %v (a clear before its add leaves the ⏳ for good)", msgID, got, want)
	}
}

// TestQueuedAck_DropWaitsForItsAdd (#1963): a queued message dropped by /new
// while its Admitted is still adding the ⏳ is cleared only after that add.
func TestQueuedAck_DropWaitsForItsAdd(t *testing.T) {
	d, rp := newReactorDispatcher(t, turn.QueueOptions{MaxDepth: 8}, &testSender{reset: func(string, bool) {}})
	adding := slowAdd(rp, "m1", 200*time.Millisecond)
	holdKey(t, d, reactorKey)

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		d.BuildHandler()(context.Background(), reactorMsg("m1", "hi"))
	}()
	<-adding // m1 is queued and its add is in flight
	d.turns.(*turn.Orchestrator).Reset(context.Background(), reactorKey, false)
	<-handled

	wantAddBeforeRemove(t, rp, "m1")
}

// TestQueuedAck_DrainedMateWaitsForItsAdd (#1963): a message drained as a
// Mate while its Admitted is still adding the ⏳ is cleared, when its turn
// ends, only after that add.
func TestQueuedAck_DrainedMateWaitsForItsAdd(t *testing.T) {
	var d *Dispatcher
	var adding <-chan struct{}
	handled := make(chan struct{})
	calls := 0
	sender := &testSender{}
	sender.getOrCreate = func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
		calls++
		if calls == 1 {
			d.BuildHandler()(context.Background(), reactorMsg("m1", "one")) // the head, its add quick
			go func() {
				defer close(handled)
				d.BuildHandler()(context.Background(), reactorMsg("m2", "two"))
			}()
			<-adding // m2 is queued and its add is in flight
		}
		return nil, 0, errors.New("turn fails at once")
	}
	d, rp := newReactorDispatcher(t, turn.QueueOptions{MaxDepth: 8, CollectDelay: time.Millisecond}, sender)
	adding = slowAdd(rp, "m2", 300*time.Millisecond)

	runIMTurn(context.Background(), d, reactorKey, "owner", reactorMsg("m0", "owner"), true)
	<-handled

	if calls != 2 {
		t.Fatalf("GetOrCreate calls = %d, want 2 (owner + one drained batch)", calls)
	}
	wantAddBeforeRemove(t, rp, "m2")
}
