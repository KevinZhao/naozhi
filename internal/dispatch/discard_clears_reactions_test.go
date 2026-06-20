package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"

	"github.com/naozhi/naozhi/internal/platform"
)

// #2013: every "queued message permanently dropped" path must clear the
// HOURGLASS reaction of the dropped messages. The two ownerLoop Discard paths
// (ctx.Done on restart, panic recovery) and the /new + /clear discardQueue
// path previously called queue.Discard which reset the ring without surfacing
// the dropped IDs, leaving ⏳ hanging forever.

// drainAndDiscardReactor wires a fakeReactorPlatform into a Dispatcher with a
// real MessageQueue so the Discard paths can be exercised end to end.
func newReactorDispatcher(t *testing.T) (*Dispatcher, *fakeReactorPlatform) {
	t.Helper()
	rp := &fakeReactorPlatform{}
	d := &Dispatcher{
		platforms: map[string]platform.Platform{"fake": rp},
		queue:     NewMessageQueue(8, 0),
	}
	return d, rp
}

func removedIDs(rp *fakeReactorPlatform) []string {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	ids := make([]string, 0, len(rp.removed))
	for _, c := range rp.removed {
		ids = append(ids, c.msgID)
	}
	return ids
}

// TestDiscardQueue_ClearsQueuedReactions covers the /new + /clear path.
func TestDiscardQueue_ClearsQueuedReactions(t *testing.T) {
	d, rp := newReactorDispatcher(t)
	const key = "im:direct:u1:general"

	// Owner + two queued follow-ups (each carrying a HOURGLASS reaction).
	d.queue.Enqueue(key, QueuedMsg{Text: "owner", MessageID: "m0"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f1", MessageID: "m1"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f2", MessageID: "m2"})

	msg := platform.IncomingMessage{Platform: "fake", ChatID: "u1"}
	d.discardQueue(context.Background(), msg, key)

	got := removedIDs(rp)
	if len(got) != 2 {
		t.Fatalf("expected 2 reactions cleared (m1, m2), got %v", got)
	}
	want := map[string]bool{"m1": true, "m2": true}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected reaction cleared: %q", id)
		}
	}
}

// TestOwnerLoopCtxDone_ClearsQueuedReactions covers the systemctl-restart
// path: the turn ctx is cancelled while follow-ups sit in the queue.
func TestOwnerLoopCtxDone_ClearsQueuedReactions(t *testing.T) {
	d, rp := newReactorDispatcher(t)
	const key = "im:direct:u1:general"

	d.queue.Enqueue(key, QueuedMsg{Text: "owner", MessageID: "m0"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f1", MessageID: "m1"})

	// Simulate the ctx.Done arm of ownerLoop's drain loop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := platform.IncomingMessage{Platform: "fake", ChatID: "u1"}
	dropped := d.queue.DiscardAndReturn(key)
	d.clearQueuedReactions(context.WithoutCancel(ctx), msg.Platform, dropped, nil)

	if got := removedIDs(rp); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("expected m1 reaction cleared on ctx.Done, got %v", got)
	}
}

// TestOwnerLoopPanic_ClearsQueuedReactions covers the panic-recovery path:
// the process survives, the platform is reachable, so reactions must clear.
func TestOwnerLoopPanic_ClearsQueuedReactions(t *testing.T) {
	d, rp := newReactorDispatcher(t)
	const key = "im:direct:u1:general"

	d.queue.Enqueue(key, QueuedMsg{Text: "owner", MessageID: "m0"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f1", MessageID: "m1"})
	d.queue.Enqueue(key, QueuedMsg{Text: "f2", MessageID: "m2"})

	msg := platform.IncomingMessage{Platform: "fake", ChatID: "u1"}
	// handleOwnerLoopPanic discards the queue and clears reactions; the
	// "请稍后重试" reply goes through replyText which is nil-safe here (no
	// sender configured -> best effort).
	d.handleOwnerLoopPanic(key, msg, "boom", nil)

	got := removedIDs(rp)
	if len(got) != 2 {
		t.Fatalf("expected 2 reactions cleared on panic (m1, m2), got %v", got)
	}
}

// TestOwnerLoopDrainPanic_ClearsDrainedBatchReactions covers
// R20260614-LOGIC-001: when a drain batch has already been pulled OUT of the
// ring by DoneOrDrain and the subsequent sendAndReply panics, the drained
// batch's HOURGLASS reactions must still be cleared. handleOwnerLoopPanic's
// DiscardAndReturn only sees the ring (now empty for the drained batch), so
// without the recover-defer's pendingClear cleanup those reactions would hang
// until the platform reaction-cache TTL (feishu: 12h), falsely telling the
// user the message is still queued.
//
// Setup: the FIRST turn's GetOrCreate returns an error (clean turn, no panic,
// no queued reaction on the owner's own message). A follow-up is enqueued so
// the drain loop pulls it out; the SECOND GetOrCreate panics, exercising the
// drained-batch path.
func TestOwnerLoopDrainPanic_ClearsDrainedBatchReactions(t *testing.T) {
	rp := &fakeReactorPlatform{}
	var calls atomic.Int64
	router := &fakeSessionRouter{
		notifyIdle: func() {},
		getOrCreate: func(_ context.Context, _ string, _ session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error) {
			n := calls.Add(1)
			if n == 1 {
				// First (owner) turn: fail cleanly so sendAndReply returns
				// via handleGetOrCreateError without panicking.
				return nil, session.SessionStatus(0), errors.New("first turn fails cleanly")
			}
			// Drain turn: panic INSIDE sendAndReply, after the batch has
			// already been removed from the ring by DoneOrDrain.
			panic("boom during drained turn")
		},
	}
	d := &Dispatcher{
		platforms: map[string]platform.Platform{"fake": rp},
		// collectDelay 0 → drain timer fires immediately.
		queue:  NewMessageQueue(8, 0),
		router: router,
		caps:   NoopCapabilities{},
	}

	const key = "im:direct:u1:general"
	msg := platform.IncomingMessage{Platform: "fake", ChatID: "u1", MessageID: "m0"}

	// Owner acquires ownership (no queued reaction on its own message).
	owner := QueuedMsg{Text: "owner", MessageID: "m0"}
	isOwner, _, _, gen, _ := d.queue.Enqueue(key, owner)
	if !isOwner {
		t.Fatalf("expected owner on first Enqueue")
	}
	// Follow-up sits in the ring carrying a HOURGLASS reaction; it will be
	// drained out by DoneOrDrain then lost to a panic in sendAndReply.
	d.queue.Enqueue(key, QueuedMsg{Text: "f1", MessageID: "m1"})

	// ownerLoop recovers the panic internally; it must not propagate.
	// Pass a non-nil logger: ownerLoop enriches it via lg.With at entry,
	// matching production wiring (BuildHandler always supplies one).
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	d.ownerLoop(context.Background(), key, gen, owner, "general", session.AgentOpts{}, msg, lg)

	// Wait briefly for any cleanup; the clear happens synchronously inside
	// the recover defer before ownerLoop returns, but poll to be robust.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(removedIDs(rp)) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	got := removedIDs(rp)
	if len(got) != 1 || got[0] != "m1" {
		t.Fatalf("expected drained batch reaction m1 cleared on panic, got %v", got)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected at least 2 GetOrCreate calls (first + drain), got %d", calls.Load())
	}
}

// TestDiscardAndReturn_ReturnsQueuedFIFO pins the queue-level contract that
// powers the fix: DiscardAndReturn surfaces the dropped messages in FIFO
// order while still tearing the queue down (Depth 0, ownership released).
func TestDiscardAndReturn_ReturnsQueuedFIFO(t *testing.T) {
	q := NewMessageQueue(8, 0)
	const key = "k"
	q.Enqueue(key, QueuedMsg{Text: "owner", MessageID: "m0"})
	q.Enqueue(key, QueuedMsg{Text: "a", MessageID: "m1"})
	q.Enqueue(key, QueuedMsg{Text: "b", MessageID: "m2"})

	dropped := q.DiscardAndReturn(key)
	if len(dropped) != 2 || dropped[0].MessageID != "m1" || dropped[1].MessageID != "m2" {
		t.Fatalf("DiscardAndReturn FIFO contract broken: %+v", dropped)
	}
	if q.Depth(key) != 0 {
		t.Errorf("depth = %d after DiscardAndReturn, want 0", q.Depth(key))
	}
	// A subsequent DiscardAndReturn on the now-empty queue returns nil.
	if again := q.DiscardAndReturn(key); again != nil {
		t.Errorf("expected nil on empty queue, got %+v", again)
	}
}
