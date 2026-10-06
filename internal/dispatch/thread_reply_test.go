package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
	"github.com/naozhi/naozhi/internal/turn"
)

const testThread = "1700000000.000042"

// threadRecPlatform records every reply like fakePlatform and adds a failing
// EditMessage and a native question card on demand.
type threadRecPlatform struct {
	fakePlatform
	editErr   error
	cardErr   error
	cards     []platform.QuestionCard
	cardChats []string
}

func (f *threadRecPlatform) EditMessage(ctx context.Context, msgID, text string) error {
	if f.editErr != nil {
		return f.editErr
	}
	return f.fakePlatform.EditMessage(ctx, msgID, text)
}

func (f *threadRecPlatform) SendQuestionCard(_ context.Context, chatID string, card platform.QuestionCard) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cardErr != nil {
		return "", f.cardErr
	}
	f.cards = append(f.cards, card)
	f.cardChats = append(f.cardChats, chatID)
	return "card-1", nil
}

func (f *threadRecPlatform) outgoing() []platform.OutgoingMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]platform.OutgoingMessage(nil), f.replies...)
}

func assistantText(s string) clievent.Event {
	return clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{
		Content: []clievent.ContentBlock{{Type: "text", Text: s}},
	}}
}

// TestThreadReply_EveryReplyKindStaysInTheThread: a message posted in a
// thread is answered in that thread by every kind of reply dispatch sends,
// not at the top level of the chat.
func TestThreadReply_EveryReplyKindStaysInTheThread(t *testing.T) {
	askEvent := clievent.Event{Type: "assistant", AskQuestion: &clievent.AskQuestion{
		ToolUseID: "tu-1",
		Items: []clievent.AskQuestionItem{{
			Question: "Which?", Options: []clievent.AskQuestionOpt{{Label: "A"}, {Label: "B"}},
		}},
	}}
	answer := func(text string, evs ...clievent.Event) func(clievent.EventCallback) (*clievent.SendResult, error) {
		return func(onEvent clievent.EventCallback) (*clievent.SendResult, error) {
			for _, ev := range evs {
				onEvent(ev)
			}
			return &clievent.SendResult{Text: text}, nil
		}
	}
	cases := []struct {
		name        string
		text        string
		interim     bool
		editErr     bool
		cardErr     bool
		sessionErr  bool
		imageReply  bool
		send        func(clievent.EventCallback) (*clievent.SendResult, error)
		wantReplies int
		wantCard    bool
	}{
		{name: "answer", send: answer("hi"), wantReplies: 1},
		{name: "split answer", send: answer(strings.Repeat("a", 9000)), wantReplies: 3},
		{name: "banner", interim: true, send: answer("hi", assistantText("working")), wantReplies: 1},
		{name: "banner edit fails", interim: true, editErr: true, send: answer("hi", assistantText("working")), wantReplies: 2},
		{name: "send error", send: func(clievent.EventCallback) (*clievent.SendResult, error) {
			return nil, errors.New("boom")
		}, wantReplies: 1},
		{name: "session error", sessionErr: true, wantReplies: 1},
		{name: "slash command", text: "/help", wantReplies: 1},
		{name: "todo", wantReplies: 2},
		{name: "question card", send: answer("bailout", askEvent), wantCard: true},
		{name: "question card fallback", cardErr: true, send: answer("bailout", askEvent), wantReplies: 1},
		{name: "image", imageReply: true, wantReplies: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &threadRecPlatform{fakePlatform: fakePlatform{supportsInterim: tc.interim}}
			send := tc.send
			if tc.name == "todo" {
				// The checklist posts from its own loop, which a finished turn
				// stops; hold the turn until it landed.
				todo := todoWriteEvent(t)
				send = func(onEvent clievent.EventCallback) (*clievent.SendResult, error) {
					onEvent(todo)
					testhelper.Eventually(t, func() bool { return fp.replyCount() > 0 }, 2*time.Second, "todo checklist posted")
					return &clievent.SendResult{Text: "done"}, nil
				}
			}
			if tc.imageReply {
				if runtime.GOOS == "darwin" {
					t.Skip("macOS /tmp resolves to /private/tmp, outside ExtractImagePaths' allowlist")
				}
				dir, err := os.MkdirTemp("/tmp", "naozhi-thread-img-*")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				img := filepath.Join(dir, "a.png")
				if err := os.WriteFile(img, []byte("png"), 0o600); err != nil {
					t.Fatal(err)
				}
				send = answer("see " + img)
			}
			if tc.editErr {
				fp.editErr = errors.New("edit refused")
			}
			if tc.cardErr {
				fp.cardErr = errors.New("card refused")
			}
			sender := &testSender{
				getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
					if tc.sessionErr {
						return nil, 0, errors.New("no session")
					}
					return fakeSession{}, session.SessionExisting, nil
				},
				send: func(_ context.Context, _ string, _ turn.Session, _ string, _ []clievent.Attachment, onEvent clievent.EventCallback) (*clievent.SendResult, error) {
					return send(onEvent)
				},
			}
			d := newTestDispatcher(&fakePlatform{}, withSender(sender))
			d.platforms = map[string]platform.Platform{"fake": fp}
			text := tc.text
			if text == "" {
				text = "question"
			}
			msg := incomingMsg(text)
			msg.ThreadID = testThread
			d.BuildHandler()(context.Background(), msg)

			out := fp.outgoing()
			if len(out) < tc.wantReplies {
				t.Fatalf("got %d replies %v, want at least %d", len(out), fp.allReplies(), tc.wantReplies)
			}
			for i, m := range out {
				if m.ChatID != "chat1" || m.ThreadID != testThread {
					t.Errorf("reply %d (%q) went to chat %q thread %q, want chat1 thread %s", i, m.Text, m.ChatID, m.ThreadID, testThread)
				}
			}
			if tc.wantCard {
				if len(fp.cards) != 1 || fp.cardChats[0] != "chat1" || fp.cards[0].ThreadID != testThread {
					t.Errorf("cards %+v to %v, want one card to chat1 in thread %s", fp.cards, fp.cardChats, testThread)
				}
			}
		})
	}
}

// TestThreadReply_TopLevelMessageHasNoThread: a message outside any thread
// is answered with an empty ThreadID, so adapters post at the top level.
func TestThreadReply_TopLevelMessageHasNoThread(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.BuildHandler()(context.Background(), incomingMsg("/help"))
	if len(fp.replies) != 1 || fp.replies[0].ThreadID != "" {
		t.Errorf("replies = %+v, want one with no ThreadID", fp.replies)
	}
}

// TestSendOutboundImages_KeepsThread pins the image send itself; the
// end-to-end image row above runs only where /tmp is not a symlink.
func TestSendOutboundImages_KeepsThread(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.sendOutboundImages(context.Background(), fp, ReplyDest{ChatID: "chat1", ThreadID: testThread},
		[]platform.Image{{Data: []byte("png"), MimeType: "image/png"}})
	if len(fp.replies) != 1 || fp.replies[0].ThreadID != testThread || len(fp.replies[0].Images) != 1 {
		t.Errorf("replies = %+v, want one image reply in thread %s", fp.replies, testThread)
	}
}

// TestThreadReply_TwoThreadsOnOneKeyAnsweredInEach: while a turn runs for
// thread T1, messages from thread T2 and T1 of the same chat queue on the
// same key. The drained turn answers both threads, each in its own thread:
// the threads are separate delivery sinks, so neither is folded into the
// other's reply.
func TestThreadReply_TwoThreadsOnOneKeyAnsweredInEach(t *testing.T) {
	const key = "fake:group:chat1:general"
	started, release := make(chan struct{}, 1), make(chan struct{})
	sender := &testSender{
		getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
			return fakeSession{}, session.SessionExisting, nil
		},
		send: func(_ context.Context, _ string, _ turn.Session, text string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
			if text == "q1" {
				started <- struct{}{}
				<-release
			}
			return &clievent.SendResult{Text: "answer to " + text}, nil
		},
	}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withSender(sender))
	submit := func(thread, id, text string) {
		m := platform.IncomingMessage{Platform: "fake", ChatType: "group", ChatID: "chat1", ThreadID: thread, MessageID: id, Text: text}
		o := d.newIMOrigin(m, slog.Default(), key, "general", session.AgentOpts{}, imMessage, len(text), 0)
		d.submit(context.Background(), o, turn.Request{Key: key, Text: text})
	}

	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		submit("T1", "m1", "q1")
	}()
	<-started
	submit("T2", "m2", "q2")
	submit("T1", "m3", "q3")
	close(release)
	select {
	case <-ownerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("owner loop did not finish")
	}

	// Answers only: the queued notice goes to the thread that queued too.
	answers := map[string][]string{}
	for _, r := range fp.replies {
		if strings.HasPrefix(r.Text, "answer to ") {
			answers[r.ThreadID] = append(answers[r.ThreadID], r.Text)
		}
	}
	if got := answers["T1"]; len(got) != 2 || got[0] != "answer to q1" {
		t.Errorf("thread T1 answers = %q, want [answer to q1, <drained turn>]", got)
	}
	if got := answers["T2"]; len(got) != 1 {
		t.Errorf("thread T2 answers = %q, want one, the drained turn", got)
	}
	if len(answers) != 2 {
		t.Errorf("answers by thread = %q, want only T1 and T2", answers)
	}
}

// TestIMOriginSink_Thread: a thread is its own sink; a top-level message
// keeps the chat's sink unchanged.
func TestIMOriginSink_Thread(t *testing.T) {
	top := &imOrigin{msg: platform.IncomingMessage{Platform: "slack", ChatType: "group", ChatID: "C1"}}
	in := &imOrigin{msg: platform.IncomingMessage{Platform: "slack", ChatType: "group", ChatID: "C1", ThreadID: "17.1"}}
	if got := top.Sink(); got != "im:slack:group:C1" {
		t.Errorf("top-level Sink = %q, want im:slack:group:C1", got)
	}
	if got := in.Sink(); got != "im:slack:group:C1#17.1" {
		t.Errorf("thread Sink = %q, want im:slack:group:C1#17.1", got)
	}
}
