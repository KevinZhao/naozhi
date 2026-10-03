package dispatch

// RETRY3 regression tests: a turn that panics must not leave the IM peer
// waiting for a reply that never arrives. The orchestrator recovers the
// panic, discards the key's queue, and the IM delivery answers with a
// Chinese "please retry" message via the same platform.Reply path used by
// the rest of dispatch.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

func testIncomingMsg() platform.IncomingMessage {
	return platform.IncomingMessage{
		Platform: "fake", EventID: "evt-panic", UserID: "u1",
		ChatID: "chat-panic", ChatType: "direct", Text: "hello",
	}
}

// panickingSender panics in GetOrCreate, after running before (if set).
func panickingSender(before func()) *testSender {
	return &testSender{getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
		if before != nil {
			before()
		}
		panic("synthetic test panic")
	}}
}

func TestTurnPanic_SendsReplyToUser(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withSender(panickingSender(nil)))
	key := session.SessionKey("fake", "direct", "chat-panic", "general")

	runIMTurn(context.Background(), d, key, "hello", testIncomingMsg(), true)

	if fp.replyCount() != 1 {
		t.Fatalf("reply count = %d, want 1 (panic notify must reach user)", fp.replyCount())
	}
	if last := fp.lastReply(); !strings.Contains(last, "处理异常") {
		t.Errorf("reply = %q, want contains %q", last, "处理异常")
	}
}

func TestTurnPanic_DiscardsQueue(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	q := turn.NewQueueWithMode(5, 0, turn.ModeCollect)
	key := session.SessionKey("fake", "direct", "chat-panic", "general")
	d := newTestDispatcher(fp, withQueue(q), withSender(panickingSender(func() {
		if _, enqueued, _, _, _ := q.Enqueue(key, turn.Msg{Text: "m2", EnqueueAt: time.Now()}); !enqueued {
			t.Error("setup: m2 was not queued behind the owner")
		}
	})))

	runIMTurn(context.Background(), d, key, "m1", testIncomingMsg(), true)

	if dropped := q.DiscardAndReturn(key); dropped != nil {
		t.Errorf("queue after panic recover still holds %d messages, want 0 (Discard not invoked)", len(dropped))
	}
	if isOwner, _, _, _, _ := q.Enqueue(key, turn.Msg{Text: "next"}); !isOwner {
		t.Error("the panicked owner still holds the key: next Enqueue did not become owner")
	}
}

func TestTurnPanic_ReplyPanicAbsorbed(t *testing.T) {
	t.Parallel()
	// Simulate a platform SDK that panics on Reply (e.g., nil chat
	// handle). The recovery must swallow this cascade so the caller's
	// goroutine is not unwound and the process can drain other owners.
	fp := &panicReplyPlatform{}
	d := newTestDispatcher(&fakePlatform{}, withSender(panickingSender(nil)))
	d.platforms = map[string]platform.Platform{"fake": fp}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nested panic escaped the turn's recovery: %v", r)
		}
	}()

	runIMTurn(context.Background(), d, "any-key", "hello", testIncomingMsg(), true)

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
