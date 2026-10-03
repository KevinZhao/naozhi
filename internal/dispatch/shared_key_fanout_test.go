package dispatch

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// TestSharedPlannerKey_EveryChatInTheBatchIsAnswered pins the IM fan-out on
// a key bound to two chats (a project planner key, which every bound chat
// resolves to). Chat A owns the loop; chat B's message, on another platform,
// queues behind it. The drained turn is answered in chat B, which asked, and
// in chat A, as Observer or as the head of its own queued message; each ⏳
// comes off on its message's own platform. A failed Send is reported in both
// chats but counted in /health once, by the Primary (chat A's delivery).
func TestSharedPlannerKey_EveryChatInTheBatchIsAnswered(t *testing.T) {
	const key = "project:p:planner"
	cases := []struct {
		name          string
		fail, aQueued bool
	}{
		{"reply", false, false},
		{"send_fails", true, false},
		{"send_fails_owner_chat_queued_too", true, true},
	}
	for _, tc := range cases {
		fail := tc.fail
		t.Run(tc.name, func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			sender := &testSender{
				getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
					return fakeSession{}, session.SessionExisting, nil
				},
				send: func(_ context.Context, _ string, _ turn.Session, text string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
					if text == "qA" {
						started <- struct{}{}
						<-release
					} else if fail {
						return nil, clierr.ErrNoOutputTimeout
					}
					return &clievent.SendResult{Text: "answer to " + text}, nil
				},
			}
			d := newTestDispatcher(&fakePlatform{}, withSender(sender))
			rpA, rpB := &fakeReactorPlatform{}, &fakeReactorPlatform{}
			d.platforms = map[string]platform.Platform{"fake": rpA, "other": rpB}
			msgOf := func(plat, chat, id, text string) platform.IncomingMessage {
				return platform.IncomingMessage{Platform: plat, ChatType: "group", ChatID: chat, MessageID: id, Text: text}
			}
			origin := func(m platform.IncomingMessage) *imOrigin {
				return d.newIMOrigin(m, slog.Default(), key, "general", session.AgentOpts{}, imMessage, len(m.Text), 0)
			}

			ownerDone := make(chan struct{})
			go func() {
				defer close(ownerDone)
				d.submit(context.Background(), origin(msgOf("fake", "chatA", "a1", "qA")), turn.Request{Key: key, Text: "qA"})
			}()
			<-started
			d.submit(context.Background(), origin(msgOf("other", "chatB", "b1", "qB")), turn.Request{Key: key, Text: "qB"})
			wantClearedA := []string{}
			if tc.aQueued {
				d.submit(context.Background(), origin(msgOf("fake", "chatA", "a2", "qA2")), turn.Request{Key: key, Text: "qA2"})
				wantClearedA = []string{"a2"}
			}
			close(release)
			select {
			case <-ownerDone:
			case <-time.After(10 * time.Second):
				t.Fatal("owner loop did not finish")
			}

			wantB := "answer to qB"
			if fail {
				wantB = "⏱️"
			}
			repliesA, repliesB := rpA.allReplies(), rpB.allReplies()
			if len(repliesA) != 2 || repliesA[0] != "answer to qA" || !strings.HasPrefix(repliesA[1], wantB) {
				t.Errorf("chat A replies = %q, want [answer to qA, %s…] (owner observes the drained turn)", repliesA, wantB)
			}
			if len(repliesB) != 1 || !strings.HasPrefix(repliesB[0], wantB) {
				t.Errorf("chat B replies = %q, want [%s…] (B asked, so B is answered)", repliesB, wantB)
			}
			for chat, rp := range map[string]*fakeReactorPlatform{"chatA": rpA, "chatB": rpB} {
				for _, r := range rp.replies {
					if r.ChatID != chat {
						t.Errorf("reply %q for %s went to %s", r.Text, chat, r.ChatID)
					}
				}
			}
			if got := removedIDs(rpB); !slices.Equal(got, []string{"b1"}) {
				t.Errorf("chat B platform cleared %v, want [b1]", got)
			}
			if got := removedIDs(rpA); !slices.Equal(got, wantClearedA) {
				t.Errorf("chat A platform cleared %v, want %v (b1 is not its message)", got, wantClearedA)
			}
			wantCount := int64(0)
			if fail {
				wantCount = 1
			}
			if n := d.replyErrorCount.Load(); n != wantCount {
				t.Errorf("replyErrorCount = %d, want %d (one turn, counted by its Primary)", n, wantCount)
			}
			if n := d.watchdogNoOutputKills.Load(); n != wantCount {
				t.Errorf("watchdogNoOutputKills = %d, want %d", n, wantCount)
			}
		})
	}
}
