package dispatch

// #3000: a turn whose message got no ⏳ posts a "💭 思考中..." banner after
// fallbackBannerDelay, also while the session is still spawning, and the
// answer is edited into it. A turn whose ⏳ landed keeps the answer as a new
// message.

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// bannerPlatform is an interim-capable ackOrderPlatform that signals each
// Reply on replied.
type bannerPlatform struct {
	ackOrderPlatform
	replied chan struct{}
}

func newBannerPlatform(addErr error) *bannerPlatform {
	p := &bannerPlatform{replied: make(chan struct{}, 8)}
	p.supportsInterim = true
	p.addErr = addErr
	return p
}

func (p *bannerPlatform) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	id, err := p.ackOrderPlatform.Reply(ctx, msg)
	p.replied <- struct{}{}
	return id, err
}

func (p *bannerPlatform) editTexts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, e := range p.edits {
		out = append(out, e.msgID+"="+e.text)
	}
	return out
}

// runBannerTurn runs one owner turn for m1 on p with the given banner delay;
// GetOrCreate waits for waitBanner (nil: no wait) before it returns.
func runBannerTurn(p *bannerPlatform, delay time.Duration, waitBanner <-chan struct{}) error {
	var spawnErr error
	sender := &testSender{
		getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
			if waitBanner != nil {
				select {
				case <-waitBanner:
				case <-time.After(5 * time.Second):
					spawnErr = errors.New("no banner while the session was spawning")
				}
			}
			p.record("session")
			return fakeSession{}, session.SessionExisting, nil
		},
		send: func(context.Context, string, turn.Session, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
			return answer()
		},
	}
	d := newTestDispatcher(&fakePlatform{}, withSender(sender))
	d.platforms = map[string]platform.Platform{"fake": p}
	d.fallbackBannerDelay = delay
	runIMTurn(context.Background(), d, reactorKey, "hi", reactorMsg("m1", "hi"), true)
	return spawnErr
}

// TestFallbackBanner_UnackedSlowTurnGetsBannerAndEditedAnswer: the ⏳ failed,
// the session takes longer than the delay, so the banner goes out before the
// session is ready and the answer replaces it.
func TestFallbackBanner_UnackedSlowTurnGetsBannerAndEditedAnswer(t *testing.T) {
	t.Parallel()
	p := newBannerPlatform(errors.New("rate limited"))
	if err := runBannerTurn(p, 10*time.Millisecond, p.replied); err != nil {
		t.Fatal(err)
	}
	if got, want := p.events(), []string{"reply", "session"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if got, want := p.allReplies(), []string{thinkingLine}; !slices.Equal(got, want) {
		t.Errorf("replies = %q, want %q", got, want)
	}
	if got, want := p.editTexts(), []string{"msg-1=answer"}; !slices.Equal(got, want) {
		t.Errorf("edits = %q, want %q", got, want)
	}
}

// TestFallbackBanner_FastTurnAnswersAsNewMessage: a turn that answers before
// the delay gets no banner; the answer is a new message.
func TestFallbackBanner_FastTurnAnswersAsNewMessage(t *testing.T) {
	t.Parallel()
	p := newBannerPlatform(errors.New("rate limited"))
	if err := runBannerTurn(p, time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := p.allReplies(), []string{"answer"}; !slices.Equal(got, want) {
		t.Errorf("replies = %q, want %q", got, want)
	}
	if got := p.editTexts(); len(got) != 0 {
		t.Errorf("edits = %q, want none", got)
	}
}

// TestFallbackBanner_ArmedOnlyForUnackedHead pins which deliveries arm the
// fallback: a head that runs at once and got no ⏳. A landed ⏳, a queued
// request (acked when it was queued) and an Observer (no message of its
// chat in the batch) do not.
func TestFallbackBanner_ArmedOnlyForUnackedHead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		reactor bool
		ack     turn.Ack
		info    turn.TurnInfo
		armed   bool
	}{
		{"owner_unacked", false, turn.AckOwner, turn.TurnInfo{Role: turn.RoleHead, First: true}, true},
		{"detached_unacked", false, turn.AckDetached, turn.TurnInfo{Role: turn.RoleHead}, true},
		{"owner_reacted", true, turn.AckOwner, turn.TurnInfo{Role: turn.RoleHead, First: true}, false},
		{"detached_reacted", true, turn.AckDetached, turn.TurnInfo{Role: turn.RoleHead}, false},
		{"queued_head", false, turn.AckQueued, turn.TurnInfo{Role: turn.RoleHead}, false},
		{"observer", false, turn.AckOwner, turn.TurnInfo{Role: turn.RoleObserver}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var p platform.Platform = &fakePlatform{supportsInterim: true}
			if tc.reactor {
				p = newBannerPlatform(nil)
			}
			d := newTestDispatcher(&fakePlatform{})
			d.platforms = map[string]platform.Platform{"fake": p}
			ctx := context.Background()
			o := d.newIMOrigin(reactorMsg("m1", "hi"), slog.Default(), reactorKey, "general", session.AgentOpts{}, imMessage, 2, 0)
			o.Admitted(ctx, tc.ack)
			dl := o.Begin(ctx, tc.info).(*imDelivery)
			dl.BeforeSession(ctx)
			defer dl.tracker.stop()
			if armed := dl.tracker.fallbackTimer != nil; armed != tc.armed {
				t.Errorf("fallback armed = %v, want %v", armed, tc.armed)
			}
		})
	}
}

// TestReplyTracker_OneBannerPerTurn: the fallback firing after an event
// already posted the banner, or after the turn claimed it, posts nothing.
func TestReplyTracker_OneBannerPerTurn(t *testing.T) {
	t.Parallel()
	t.Run("event_first", func(t *testing.T) {
		t.Parallel()
		fp := &fakePlatform{supportsInterim: true}
		tr := newIMEventTracker(context.Background(), fp, "chat1", "direct", "")
		tr.armFallbackBanner(time.Hour)
		tr.onEvent(clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{
			Content: []clievent.ContentBlock{{Type: "text", Text: "working"}}}})
		tr.postBanner() // the timer firing late
		tr.waitReady(context.Background())
		tr.stop()
		if got, want := fp.allReplies(), []string{thinkingLine}; !slices.Equal(got, want) {
			t.Errorf("replies = %q, want %q", got, want)
		}
	})
	t.Run("after_stop", func(t *testing.T) {
		t.Parallel()
		fp := &fakePlatform{supportsInterim: true}
		tr := newIMEventTracker(context.Background(), fp, "chat1", "direct", "")
		tr.armFallbackBanner(time.Hour)
		tr.stop()
		if tr.fallbackTimer.Stop() {
			t.Error("stop left the fallback timer running")
		}
		tr.postBanner() // a timer that fired as stop ran
		<-tr.msgIDReady // closed by stop, or after a Reply it let through
		if got := fp.allReplies(); len(got) != 0 {
			t.Errorf("replies after stop = %q, want none", got)
		}
	})
}

// gatedReplyPlatform blocks Reply until release is closed.
type gatedReplyPlatform struct {
	fakePlatform
	entered chan struct{}
	release chan struct{}
}

func (p *gatedReplyPlatform) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	close(p.entered)
	<-p.release
	return p.fakePlatform.Reply(ctx, msg)
}

// TestReplyTracker_StopWaitsForBannerInFlight: a banner Reply still in
// flight when the turn ends (an error path never calls waitReady) finishes
// before stop returns, so it cannot post into the next turn.
func TestReplyTracker_StopWaitsForBannerInFlight(t *testing.T) {
	t.Parallel()
	p := &gatedReplyPlatform{entered: make(chan struct{}), release: make(chan struct{})}
	p.supportsInterim = true
	tr := newIMEventTracker(context.Background(), p, "chat1", "direct", "")
	tr.armFallbackBanner(time.Millisecond)
	<-p.entered
	stopped := make(chan struct{})
	go func() {
		tr.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while the banner Reply was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(p.release)
	<-stopped
	if got := p.allReplies(); !slices.Equal(got, []string{thinkingLine}) {
		t.Errorf("replies = %q, want %q", got, []string{thinkingLine})
	}
}
