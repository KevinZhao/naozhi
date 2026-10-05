package dispatch

// #3000: the message that starts an owner loop is acked with a ⏳, and the ⏳
// comes off once the turn has answered, on every way the turn can end. The
// add runs beside the turn (#3329), so only its place before the removal is
// fixed.

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
// GetOrCreate in the order they happen. A non-nil addGate holds every
// AddReaction until it is closed.
type ackOrderPlatform struct {
	fakePlatform
	logMu      sync.Mutex
	log        []string
	addErr     error
	addGate    chan struct{}
	replyPanic bool
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
	if p.replyPanic {
		panic("synthetic platform Reply panic")
	}
	return p.fakePlatform.Reply(ctx, msg)
}

func (p *ackOrderPlatform) AddReaction(ctx context.Context, id string, _ platform.ReactionType) error {
	if p.addGate != nil {
		select {
		case <-p.addGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	runTurnOn(p, rec, getOrCreate, send, true)
}

// runTurnOn is runOwnerTurn for an owner turn when first, else a detached
// PriorityNow turn.
func runTurnOn(p platform.Platform, rec func(string), getOrCreate func() (turn.Session, session.SessionStatus, error), send func() (*clievent.SendResult, error), first bool) {
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
	runIMTurn(context.Background(), d, reactorKey, "hi", reactorMsg("m1", "hi"), first)
}

// checkAckOrder reports got unless it is want with "add:m1" once somewhere
// before "remove:m1".
func checkAckOrder(t *testing.T, got, want []string) {
	t.Helper()
	add := slices.Index(got, "add:m1")
	rest := slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s == "add:m1" })
	if add < 0 || len(rest) != len(got)-1 || add > slices.Index(got, "remove:m1") || !slices.Equal(rest, want) {
		t.Errorf("events = %v, want %v with add:m1 once before remove:m1", got, want)
	}
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
			[]string{"session", "reply", "remove:m1"}},
		{"session_error", func() (turn.Session, session.SessionStatus, error) {
			return nil, 0, errors.New("spawn failed")
		}, answer,
			[]string{"session", "reply", "remove:m1"}},
		{"send_error", existingSession, func() (*clievent.SendResult, error) {
			return nil, clierr.ErrNoOutputTimeout
		}, []string{"session", "reply", "remove:m1"}},
		// A panic clears the ⏳ before the generic retry notice.
		{"panic", func() (turn.Session, session.SessionStatus, error) {
			panic("boom")
		}, answer,
			[]string{"session", "remove:m1", "reply"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &ackOrderPlatform{}
			runOwnerTurn(p, p.record, tc.getOrCreate, tc.send)
			checkAckOrder(t, p.events(), tc.want)
		})
	}
}

// TestOwnerTurn_ReplyPanicStillClearsReaction: the turn layer does not call a
// Finish that panicked again, so Finish itself must clear the ⏳.
func TestOwnerTurn_ReplyPanicStillClearsReaction(t *testing.T) {
	t.Parallel()
	p := &ackOrderPlatform{replyPanic: true}
	runOwnerTurn(p, p.record, existingSession, answer)
	checkAckOrder(t, p.events(), []string{"session", "reply", "remove:m1"})
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
