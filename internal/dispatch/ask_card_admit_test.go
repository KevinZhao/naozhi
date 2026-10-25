package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// cardClick is a question card answer from user in group chat "g", counting
// its OnAccepted calls into accepted.
func cardClick(id, user, asker string, accepted *int) platform.IncomingMessage {
	m := scopeMsg(id, "group", user, "", "Style: Errors.")
	m.AskerID = asker
	m.OnAccepted = func() { *accepted++ }
	return m
}

// TestAskCard_AskerOnlyInAMembersSession: a card asked in a group member's
// own session (group_scope user) names them as the only one who may answer;
// a card in a shared session names no one.
func TestAskCard_AskerOnlyInAMembersSession(t *testing.T) {
	ask := clievent.Event{Type: "assistant", AskQuestion: &clievent.AskQuestion{
		ToolUseID: "tu-1",
		Items: []clievent.AskQuestionItem{{
			Question: "Which?", Options: []clievent.AskQuestionOpt{{Label: "A"}, {Label: "B"}},
		}},
	}}
	for _, tc := range []struct {
		name     string
		scope    GroupScope
		chatType string
		want     string
	}{
		{"user scope group", GroupScopeUser, "group", "u1"},
		{"thread scope", GroupScopeThread, "group", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &threadRecPlatform{}
			sender := &testSender{
				getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
					return fakeSession{}, session.SessionExisting, nil
				},
				send: func(_ context.Context, _ string, _ turn.Session, _ string, _ []clievent.Attachment, onEvent clievent.EventCallback) (*clievent.SendResult, error) {
					onEvent(ask)
					return &clievent.SendResult{Text: "bailout"}, nil
				},
			}
			d := newTestDispatcher(&fakePlatform{}, withSender(sender), withGroupScope(tc.scope))
			d.platforms = map[string]platform.Platform{"fake": fp}
			d.BuildHandler()(context.Background(), scopeMsg("m1", tc.chatType, "u1", "", "question"))
			fp.mu.Lock()
			defer fp.mu.Unlock()
			if len(fp.cards) != 1 {
				t.Fatalf("cards = %+v, want one", fp.cards)
			}
			if got := fp.cards[0].AskerID; got != tc.want {
				t.Errorf("card AskerID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAskCard_OnlyTheAskerAnswers: another member's click on a card asked in
// the asker's own session gets no turn and is told why; the adapter is never
// told it was accepted, so the card keeps its buttons. The asker's click,
// and any click on a card with no asker, are taken.
func TestAskCard_OnlyTheAskerAnswers(t *testing.T) {
	d, fp, ct := newRateLimitDispatcher(t, RateLimit{})
	d.groupScope = GroupScopeUser
	h := d.BuildHandler()

	var refused int
	h(context.Background(), cardClick("c1", "u2", "u1", &refused))
	if refused != 0 || ct.submits.Load() != 0 {
		t.Errorf("another member's click: accepted %d, submits %d; want neither", refused, ct.submits.Load())
	}
	if n := countReplies(fp, notAskerReply); n != 1 {
		t.Errorf("not-the-asker replies = %d, want 1: %q", n, fp.allReplies())
	}

	var asker, open int
	h(context.Background(), cardClick("c2", "u1", "u1", &asker))
	h(context.Background(), cardClick("c3", "u2", "", &open))
	if asker != 1 || open != 1 {
		t.Errorf("accepted: asker %d, open card %d; want 1 each", asker, open)
	}
}

// TestAskCard_RefusedClickIsNotAccepted: a click a gate refuses (here the
// rate limit) never reports OnAccepted, so the card keeps its buttons.
func TestAskCard_RefusedClickIsNotAccepted(t *testing.T) {
	d, _, _ := newRateLimitDispatcher(t, RateLimit{MsgsPerMin: 1, Burst: 1})
	h := d.BuildHandler()
	var first, second int
	h(context.Background(), cardClick("c1", "u1", "", &first))
	h(context.Background(), cardClick("c2", "u1", "", &second))
	if first != 1 || second != 0 {
		t.Errorf("accepted %d then %d, want 1 then 0", first, second)
	}
}
