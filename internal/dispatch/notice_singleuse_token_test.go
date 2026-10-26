package dispatch

// #3003: a text notice that is not the turn's answer must never spend a
// single-use reply token (WeChat/iLink): the token cached for the user is the
// one the real answer is sent on, so a "please wait" would swallow it.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/turn"
)

// fakeSingleUseReactorless is a single-use-token platform that is NOT a
// Reactor (Weixin-like): no AddReaction surface, so reaction acks fall
// through to text and every Reply is recorded. limit is MaxReplyLength
// (0 means 4000).
type fakeSingleUseReactorless struct {
	mu      sync.Mutex
	limit   int
	replies []platform.OutgoingMessage
}

func (f *fakeSingleUseReactorless) Name() string                                               { return "fake-su" }
func (f *fakeSingleUseReactorless) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (f *fakeSingleUseReactorless) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, msg)
	return "msg-1", nil
}
func (f *fakeSingleUseReactorless) EditMessage(_ context.Context, _, _ string) error { return nil }
func (f *fakeSingleUseReactorless) MaxReplyLength() int {
	if f.limit == 0 {
		return 4000
	}
	return f.limit
}
func (f *fakeSingleUseReactorless) SupportsInterimMessages() bool { return false }
func (f *fakeSingleUseReactorless) UsesSingleUseReplyToken() bool { return true }
func (f *fakeSingleUseReactorless) replyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replies)
}

// fakeSingleUseInterim is a single-use, non-Reactor platform that claims
// interim support, so the resume-lost notice reaches the single-use gate
// rather than stopping at SupportsInterimMessages.
type fakeSingleUseInterim struct{ fakeSingleUseReactorless }

func (f *fakeSingleUseInterim) SupportsInterimMessages() bool { return true }

// noticeSite triggers one non-answer notice for chat1 on platform name.
// rateLimited sites spend chat1's ShouldNotify cooldown when they send.
type noticeSite struct {
	name        string
	text        string
	rateLimited bool
	queue       *turn.QueueOptions // nil: the default collect-mode queue
	fire        func(t *testing.T, d *Dispatcher, name string)
}

func noticeKey(name string) string { return session.SessionKey(name, "direct", "chat1", "general") }

func noticeMsg(name string) platform.IncomingMessage {
	return platform.IncomingMessage{
		Platform: name, EventID: "evt-2", MessageID: "m2",
		UserID: "user1", ChatID: "chat1", ChatType: "direct", Text: "second",
	}
}

func fireBusyMessage(t *testing.T, d *Dispatcher, name string) {
	holdKey(t, d, noticeKey(name))
	d.BuildHandler()(context.Background(), noticeMsg(name))
}

// noticeSites lists every notice dispatch sends mid-turn that is not the
// turn's answer. A new one goes through replyNotice and gets a row here.
var noticeSites = []noticeSite{
	{name: "queued", text: "消息已收到", rateLimited: true, fire: fireBusyMessage},
	{name: "busy", text: "正在处理", rateLimited: true, queue: &turn.QueueOptions{}, fire: fireBusyMessage},
	{name: "merged", text: "已合并到上一条回复", rateLimited: true, fire: func(_ *testing.T, d *Dispatcher, name string) {
		d.ackMergedFollower(context.Background(), noticeMsg(name), noticeKey(name), 2, nil)
	}},
	{name: "resume_lost", text: "之前的会话记录已丢失", fire: func(_ *testing.T, d *Dispatcher, name string) {
		fireSessionReady(d, name, sessionview.SessionResumeLost)
	}},
}

// fireSessionReady runs one first-turn delivery's SessionReady with st.
func fireSessionReady(d *Dispatcher, name string, st sessionview.SessionStatus) {
	msg := noticeMsg(name)
	o := d.newIMOrigin(msg, slog.Default(), noticeKey(name), "general", session.AgentOpts{}, imMessage, len(msg.Text), 0)
	dl := &imDelivery{o: o, info: turn.TurnInfo{First: true}, p: d.platforms[name], lg: slog.Default()}
	dl.BeforeSession(context.Background())
	dl.SessionReady(context.Background(), st)
	dl.tracker.stop()
}

func noticeDispatcher(site noticeSite, p platform.Platform) *Dispatcher {
	var opts []dispatcherTestOption
	if site.queue != nil {
		opts = append(opts, withQueue(*site.queue))
	}
	d := newTestDispatcher(&fakePlatform{}, opts...)
	d.platforms = map[string]platform.Platform{p.Name(): p}
	return d
}

// TestNotices_SingleUseTokenGate: on a single-use platform no notice site
// sends anything or spends the cooldown; on a multi-send one each sends
// exactly its notice, so the gate is not over-broad, and a rate-limited one
// spends the chat's cooldown.
func TestNotices_SingleUseTokenGate(t *testing.T) {
	t.Parallel()
	for _, site := range noticeSites {
		t.Run(site.name+"/single_use", func(t *testing.T) {
			t.Parallel()
			fp := &fakeSingleUseInterim{}
			d := noticeDispatcher(site, fp)
			site.fire(t, d, fp.Name())
			if n := fp.replyCount(); n != 0 {
				t.Errorf("%s notice spent the single-use token: %d Reply calls, want 0", site.name, n)
			}
			if !d.turns.ShouldNotify(noticeKey(fp.Name())) {
				t.Errorf("a skipped %s notice spent the cooldown", site.name)
			}
		})
		t.Run(site.name+"/multi_send", func(t *testing.T) {
			t.Parallel()
			fp := &fakePlatform{supportsInterim: true}
			d := noticeDispatcher(site, fp)
			site.fire(t, d, fp.Name())
			if got := fp.allReplies(); len(got) != 1 || !strings.Contains(got[0], site.text) {
				t.Errorf("%s notice on a multi-send platform = %q, want exactly one containing %q", site.name, got, site.text)
			}
			if spent := !d.turns.ShouldNotify(noticeKey(fp.Name())); spent != site.rateLimited {
				t.Errorf("%s notice spent the cooldown = %v, want %v", site.name, spent, site.rateLimited)
			}
		})
	}
}
