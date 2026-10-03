package dispatch

// #3003: a text notice that is not the turn's answer must never spend a
// single-use reply token (WeChat/iLink): the token cached for the user is the
// one the real answer is sent on, so a "please wait" would swallow it.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

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
			fp := &fakeSingleUseReactorless{}
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

// TestAdmittedDropped_LogsTheDrop: with the busy notice gated, the dropped
// message must still leave an Info line saying nobody was told.
func TestAdmittedDropped_LogsTheDrop(t *testing.T) {
	t.Parallel()
	fp := &fakeSingleUseReactorless{}
	d := newTestDispatcher(&fakePlatform{}, withQueue(turn.QueueOptions{}))
	d.platforms = map[string]platform.Platform{fp.Name(): fp}
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	o := d.newIMOrigin(noticeMsg(fp.Name()), lg, "k1", "general", session.AgentOpts{}, imMessage, 6, 0)

	o.Admitted(context.Background(), turn.AckDropped)

	if out := buf.String(); !strings.Contains(out, "message dropped: session busy") || !strings.Contains(out, "notified=false") {
		t.Errorf("AckDropped on a single-use platform logged %q; want the drop at Info with notified=false", out)
	}
	if n := fp.replyCount(); n != 0 {
		t.Errorf("busy notice sent %d replies on a single-use platform; want 0", n)
	}
}
