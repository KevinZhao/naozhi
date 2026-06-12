package dispatch

import (
	"context"
	"testing"

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
