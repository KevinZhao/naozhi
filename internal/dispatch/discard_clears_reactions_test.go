package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// #2013: every "queued message permanently dropped" path must clear the
// HOURGLASS reaction of the dropped messages: /new and /clear (Reset), the
// owner loop exiting on ctx.Done (restart), and panic recovery, including a
// drained batch the panic caught in flight. These run through the real
// turn.Orchestrator, so each path is the one production takes.

const reactorKey = "fake:direct:chat1:general"

// newReactorDispatcher wires a fakeReactorPlatform into a Dispatcher whose
// Turns run on q through sender.
func newReactorDispatcher(t *testing.T, q *turn.Queue, sender *testSender) (*Dispatcher, *fakeReactorPlatform) {
	t.Helper()
	rp := &fakeReactorPlatform{}
	d := newTestDispatcher(&fakePlatform{}, withQueue(q), withSender(sender))
	d.platforms = map[string]platform.Platform{"fake": rp}
	return d, rp
}

// reactorMsg is an inbound message on the reactor chat carrying platform
// message ID id.
func reactorMsg(id, text string) platform.IncomingMessage {
	m := incomingMsg(text)
	m.EventID, m.MessageID = "evt-"+id, id
	return m
}

// queueIM parks one IM request per id behind whoever owns reactorKey, each
// with its own imOrigin, as Submit would for a message that arrived busy.
func queueIM(d *Dispatcher, q *turn.Queue, ids ...string) {
	for _, id := range ids {
		o := d.newIMOrigin(reactorMsg(id, id), slog.Default(), reactorKey, "general", session.AgentOpts{}, imMessage, len(id), 0)
		q.Enqueue(reactorKey, turn.Msg{Text: id, MessageID: id, Origin: o})
	}
}

func removedIDs(rp *fakeReactorPlatform) []string {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	ids := make([]string, 0, len(rp.removed))
	for _, c := range rp.removed {
		ids = append(ids, c.msgID)
	}
	slices.Sort(ids)
	return ids
}

func wantRemoved(t *testing.T, rp *fakeReactorPlatform, want ...string) {
	t.Helper()
	if got := removedIDs(rp); !slices.Equal(got, want) {
		t.Fatalf("reactions cleared = %v, want %v", got, want)
	}
}

// TestReset_ClearsQueuedReactions covers the /new + /clear path.
func TestReset_ClearsQueuedReactions(t *testing.T) {
	q := turn.NewQueueWithMode(8, 0, turn.ModeCollect)
	d, rp := newReactorDispatcher(t, q, &testSender{})
	q.Enqueue(reactorKey, turn.Msg{Text: "owner"}) // the running owner
	queueIM(d, q, "m1", "m2")

	d.BuildHandler()(context.Background(), reactorMsg("m3", "/new"))

	wantRemoved(t, rp, "m1", "m2")
}

// TestOwnerLoopCtxDone_ClearsQueuedReactions covers the systemctl-restart
// path: the turn ctx is cancelled while a follow-up sits in the queue. The
// hour-long collect delay leaves ctx.Done as the loop's only way out.
func TestOwnerLoopCtxDone_ClearsQueuedReactions(t *testing.T) {
	q := turn.NewQueueWithMode(8, time.Hour, turn.ModeCollect)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var d *Dispatcher
	sender := &testSender{}
	sender.getOrCreate = func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
		queueIM(d, q, "m1")
		cancel()
		return nil, 0, errors.New("first turn fails cleanly")
	}
	d, rp := newReactorDispatcher(t, q, sender)

	runIMTurn(ctx, d, reactorKey, "owner", reactorMsg("m0", "owner"), true)

	wantRemoved(t, rp, "m1")
}

// TestOwnerLoopPanic_ClearsQueuedReactions covers the panic-recovery path:
// the process survives, the platform is reachable, so the reactions of the
// messages the panic dropped must clear — and the owner's own message, which
// never got one, is left alone.
func TestOwnerLoopPanic_ClearsQueuedReactions(t *testing.T) {
	q := turn.NewQueueWithMode(8, 0, turn.ModeCollect)
	var d *Dispatcher
	sender := &testSender{}
	sender.getOrCreate = func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
		queueIM(d, q, "m1", "m2")
		panic("boom")
	}
	d, rp := newReactorDispatcher(t, q, sender)

	runIMTurn(context.Background(), d, reactorKey, "owner", reactorMsg("m0", "owner"), true)

	wantRemoved(t, rp, "m1", "m2")
}

// TestOwnerLoopDrainPanic_ClearsDrainedBatchReactions covers
// R20260614-LOGIC-001: a drain batch is already out of the ring when its
// turn panics, so the queue discard cannot see it; the turn's own delivery
// must still clear its HOURGLASS reactions, or they hang until the platform
// reaction-cache TTL (feishu: 12h). The first turn fails cleanly; the drain
// turn's GetOrCreate panics.
func TestOwnerLoopDrainPanic_ClearsDrainedBatchReactions(t *testing.T) {
	q := turn.NewQueueWithMode(8, 0, turn.ModeCollect)
	var d *Dispatcher
	var calls atomic.Int64
	sender := &testSender{}
	sender.getOrCreate = func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
		if calls.Add(1) == 1 {
			queueIM(d, q, "m1")
			return nil, 0, errors.New("first turn fails cleanly")
		}
		panic("boom during drained turn")
	}
	d, rp := newReactorDispatcher(t, q, sender)

	runIMTurn(context.Background(), d, reactorKey, "owner", reactorMsg("m0", "owner"), true)

	if calls.Load() != 2 {
		t.Fatalf("GetOrCreate calls = %d, want 2 (first + drain)", calls.Load())
	}
	wantRemoved(t, rp, "m1")
}

// TestDetachedTurn_ClearsItsOwnReaction pins #1946: a passthrough or /urgent
// turn never enters a drain batch, so its delivery clears the ⏳ its
// admission put on the message.
func TestDetachedTurn_ClearsItsOwnReaction(t *testing.T) {
	q := turn.NewQueueWithMode(8, 0, turn.ModePassthrough)
	d, rp := newReactorDispatcher(t, q, &testSender{
		send: func(context.Context, string, turn.Session, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
			return nil, errors.New("fast fail")
		},
		getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
			return fakeSession{}, session.SessionExisting, nil
		},
	})
	o := d.newIMOrigin(reactorMsg("m1", "hi"), slog.Default(), reactorKey, "general", session.AgentOpts{}, imMessage, 2, 0)
	d.turns.Submit(context.Background(), turn.Request{Key: reactorKey, Text: "hi", Origin: o}, inlineAdmission{context.Background()})

	rp.mu.Lock()
	added := len(rp.added)
	rp.mu.Unlock()
	if added != 1 {
		t.Fatalf("detached admission added %d reactions, want 1", added)
	}
	wantRemoved(t, rp, "m1")
}

// fakeSession is a turn.Session for tests whose Sender never reaches a real
// session.
type fakeSession struct{}

func (fakeSession) Backend() string { return "claude" }

// TestDiscardAndReturn_ReturnsQueuedFIFO pins the queue-level contract that
// powers the fix: DiscardAndReturn surfaces the dropped messages in FIFO
// order while still tearing the queue down (ring emptied, ownership released).
func TestDiscardAndReturn_ReturnsQueuedFIFO(t *testing.T) {
	q := turn.NewQueueWithMode(8, 0, turn.ModeCollect)
	const key = "k"
	q.Enqueue(key, turn.Msg{Text: "owner", MessageID: "m0"})
	q.Enqueue(key, turn.Msg{Text: "a", MessageID: "m1"})
	q.Enqueue(key, turn.Msg{Text: "b", MessageID: "m2"})

	dropped := q.DiscardAndReturn(key)
	if len(dropped) != 2 || dropped[0].MessageID != "m1" || dropped[1].MessageID != "m2" {
		t.Fatalf("DiscardAndReturn FIFO contract broken: %+v", dropped)
	}
	// A subsequent DiscardAndReturn on the now-empty queue returns nil.
	if again := q.DiscardAndReturn(key); again != nil {
		t.Errorf("expected nil on empty queue, got %+v", again)
	}
	// Ownership was released: the next Enqueue becomes owner instead of
	// queueing behind the discarded turn.
	if isOwner, _, _, _, _ := q.Enqueue(key, turn.Msg{Text: "next"}); !isOwner {
		t.Error("DiscardAndReturn did not release ownership: next Enqueue did not become owner")
	}
}
