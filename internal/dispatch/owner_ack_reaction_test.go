package dispatch

// #3000: the message that starts an owner loop is acked with a ⏳ before its
// turn runs, and the ⏳ comes off once the turn has answered, on every way
// the turn can end.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// ackOrderPlatform is a Reactor that logs replies, reactions and the turn's
// GetOrCreate in the order they happen.
type ackOrderPlatform struct {
	fakePlatform
	logMu  sync.Mutex
	log    []string
	addErr error
}

func (p *ackOrderPlatform) record(s string) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	p.log = append(p.log, s)
}

func (p *ackOrderPlatform) events() []string {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	return slices.Clone(p.log)
}

func (p *ackOrderPlatform) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	p.record("reply")
	return p.fakePlatform.Reply(ctx, msg)
}

func (p *ackOrderPlatform) AddReaction(_ context.Context, id string, _ platform.ReactionType) error {
	if p.addErr != nil {
		return p.addErr
	}
	p.record("add:" + id)
	return nil
}

func (p *ackOrderPlatform) RemoveReaction(_ context.Context, id string, _ platform.ReactionType) error {
	p.record("remove:" + id)
	return nil
}

// runOwnerTurn runs one owner-loop turn for message m1 on p, logging
// GetOrCreate as "session".
func runOwnerTurn(p platform.Platform, rec func(string), getOrCreate func() (turn.Session, session.SessionStatus, error), send func() (*clievent.SendResult, error)) {
	sender := &testSender{
		getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
			rec("session")
			return getOrCreate()
		},
		send: func(context.Context, string, turn.Session, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
			return send()
		},
	}
	d := newTestDispatcher(&fakePlatform{}, withSender(sender))
	d.platforms = map[string]platform.Platform{"fake": p}
	runIMTurn(context.Background(), d, reactorKey, "hi", reactorMsg("m1", "hi"), true)
}

func existingSession() (turn.Session, session.SessionStatus, error) {
	return fakeSession{}, session.SessionExisting, nil
}

func answer() (*clievent.SendResult, error) { return &clievent.SendResult{Text: "answer"}, nil }

func TestOwnerTurn_AcksWithReactionAndClearsItAfterTheReply(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		getOrCreate func() (turn.Session, session.SessionStatus, error)
		send        func() (*clievent.SendResult, error)
		want        []string
	}{
		{"answer", existingSession, answer,
			[]string{"add:m1", "session", "reply", "remove:m1"}},
		{"session_error", func() (turn.Session, session.SessionStatus, error) {
			return nil, 0, errors.New("spawn failed")
		}, answer,
			[]string{"add:m1", "session", "reply", "remove:m1"}},
		{"send_error", existingSession, func() (*clievent.SendResult, error) {
			return nil, clierr.ErrNoOutputTimeout
		}, []string{"add:m1", "session", "reply", "remove:m1"}},
		// A panic clears the ⏳ before the generic retry notice.
		{"panic", func() (turn.Session, session.SessionStatus, error) {
			panic("boom")
		}, answer,
			[]string{"add:m1", "session", "remove:m1", "reply"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &ackOrderPlatform{}
			runOwnerTurn(p, p.record, tc.getOrCreate, tc.send)
			if got := p.events(); !slices.Equal(got, tc.want) {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOwnerTurn_NoReactionNoFallbackText: where the ⏳ cannot land (no
// Reactor, or the API fails), the chat gets the answer and nothing else, and
// no removal is attempted for a ⏳ that is not there.
func TestOwnerTurn_NoReactionNoFallbackText(t *testing.T) {
	t.Parallel()
	t.Run("not_a_reactor", func(t *testing.T) {
		t.Parallel()
		fp := &fakePlatform{}
		runOwnerTurn(fp, func(string) {}, existingSession, answer)
		if got := fp.allReplies(); !slices.Equal(got, []string{"answer"}) {
			t.Errorf("replies = %q, want [answer]", got)
		}
	})
	t.Run("add_fails", func(t *testing.T) {
		t.Parallel()
		p := &ackOrderPlatform{addErr: errors.New("rate limited")}
		runOwnerTurn(p, p.record, existingSession, answer)
		if got, want := p.events(), []string{"session", "reply"}; !slices.Equal(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
		if got := p.allReplies(); !slices.Equal(got, []string{"answer"}) {
			t.Errorf("replies = %q, want [answer]", got)
		}
	})
}
