package dispatch

// RETRY3 regression tests: a turn that panics must not leave the IM peer
// waiting for a reply that never arrives. The orchestrator recovers the
// panic, discards the key's queue, and the IM delivery answers with a
// Chinese "please retry" message via the same platform.Reply path used by
// the rest of dispatch.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
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
	key := session.SessionKey("fake", "direct", "chat-panic", "general")
	ctx := context.Background()
	var d *Dispatcher
	var panicked bool
	var sent []string // the texts of the turns after the panic
	sender := &testSender{
		getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
			if !panicked {
				panicked = true
				if ack := d.turns.Submit(ctx, turn.Request{Key: key, Text: "m2"}, parkedAdmission{}); ack != turn.AckQueued {
					t.Errorf("setup: m2 ack %d, want queued behind the owner", ack)
				}
				panic("synthetic test panic")
			}
			return fakeSession{}, session.SessionExisting, nil
		},
		send: func(_ context.Context, _ string, _ turn.Session, text string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
			sent = append(sent, text)
			return &clievent.SendResult{Text: "ok"}, nil
		},
	}
	d = newTestDispatcher(fp, withSender(sender))

	runIMTurn(ctx, d, key, "m1", testIncomingMsg(), true)

	// The panicked owner released the key, so the next message owns it; and
	// the recovery discarded m2, so no drain turn after it carries m2.
	if ack := d.turns.Submit(ctx, turn.Request{Key: key, Text: "next"}, inlineAdmission{ctx}); ack != turn.AckOwner {
		t.Fatalf("next message ack %d after the panic, want AckOwner (the panicked owner still holds the key)", ack)
	}
	if !slices.Equal(sent, []string{"next"}) {
		t.Errorf("turns after the panic sent %q, want only [next] (m2 was not discarded)", sent)
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
