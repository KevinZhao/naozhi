package dispatch

// RETRY3 regression tests. Before Round 97 the ownerLoop panic recover
// only logged + Discard'd the queue — the IM peer was left waiting for a
// reply that would never arrive. The handleOwnerLoopPanic helper now
// also sends a Chinese "please retry" message via the same platform.Reply
// path used by the rest of dispatch.
//
// These tests exercise the helper directly because an end-to-end
// ownerLoop panic is hard to construct in unit tests: router.GetOrCreate
// fails before reaching sendFn under the test harness (no real wrapper),
// so a panicking sendFn never runs. The extracted helper lets us pin the
// three post-recover behaviours (log, Discard, reply) with a minimal
// stub.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

func testIncomingMsg() platform.IncomingMessage {
	return platform.IncomingMessage{
		Platform: "fake", EventID: "evt-panic", UserID: "u1",
		ChatID: "chat-panic", ChatType: "direct", Text: "hello",
	}
}

func TestHandleOwnerLoopPanic_SendsReplyToUser(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.queue = NewMessageQueue(5, 0)

	key := session.SessionKey("fake", "direct", "chat-panic", "general")
	msg := testIncomingMsg()

	// Invoke the recover helper with a synthetic panic value.
	d.handleOwnerLoopPanic(key, msg, "synthetic test panic")

	if fp.replyCount() != 1 {
		t.Fatalf("reply count = %d, want 1 (panic notify must reach user)", fp.replyCount())
	}
	if last := fp.lastReply(); !strings.Contains(last, "处理异常") {
		t.Errorf("reply = %q, want contains %q", last, "处理异常")
	}
}

func TestHandleOwnerLoopPanic_DiscardsQueue(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.queue = NewMessageQueue(5, 0)

	key := session.SessionKey("fake", "direct", "chat-panic", "general")
	// Seed queued messages so we can verify Discard clears them.
	d.queue.Enqueue(key, QueuedMsg{Text: "m1", EnqueueAt: time.Now()})
	d.queue.Enqueue(key, QueuedMsg{Text: "m2", EnqueueAt: time.Now()})
	if depth := d.queue.Depth(key); depth == 0 {
		t.Fatalf("setup: expected nonzero depth, got %d", depth)
	}

	d.handleOwnerLoopPanic(key, testIncomingMsg(), "synthetic test panic")

	if depth := d.queue.Depth(key); depth != 0 {
		t.Errorf("queue depth after panic recover = %d, want 0 (Discard not invoked)", depth)
	}
}

func TestHandleOwnerLoopPanic_NilQueueNoCrash(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.queue = nil // Guard-based deployments leave queue unset.

	// Must not panic on nil queue. The reply still goes out.
	d.handleOwnerLoopPanic("any-key", testIncomingMsg(), "synthetic test panic")

	if fp.replyCount() != 1 {
		t.Errorf("reply count = %d, want 1 even with nil queue", fp.replyCount())
	}
}

func TestHandleOwnerLoopPanic_ReplyPanicAbsorbed(t *testing.T) {
	t.Parallel()
	// Simulate a platform SDK that panics on Reply (e.g., nil chat
	// handle). The nested recover inside handleOwnerLoopPanic must
	// swallow this cascade so the caller's outer defer is not unwound
	// and the process can drain other owners.
	fp := &panicReplyPlatform{}
	d := newTestDispatcher(nil, nil) // base dispatcher without fake platform
	d.platforms = map[string]platform.Platform{"fake": fp}
	d.queue = NewMessageQueue(5, 0)

	// This call must not re-panic past the test frame.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nested panic escaped handleOwnerLoopPanic: %v", r)
		}
	}()
	d.handleOwnerLoopPanic("any-key", testIncomingMsg(), "synthetic test panic")
	if !fp.called {
		t.Errorf("panic-notifying Reply was not attempted")
	}
}

// panicReplyPlatform satisfies platform.Platform but panics on Reply to
// simulate a buggy SDK. Only Reply is exercised by the panic-notify path.
type panicReplyPlatform struct {
	called bool
	fakePlatform
}

func (p *panicReplyPlatform) Reply(_ context.Context, _ platform.OutgoingMessage) (string, error) {
	p.called = true
	panic("synthetic platform Reply panic")
}
