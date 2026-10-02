package dispatch

import (
	"context"
	"errors"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/turn"
)

// routerOf wraps r for DispatcherConfig.Router; nil stays an untyped nil.
func routerOf(r *session.Router) SessionRouter {
	if r == nil {
		return nil
	}
	return r
}

// testSender is the turn.Sender dispatch tests run IM turns through, playing
// server's turnSender. A nil hook falls back to router (when set), the way
// production reaches *session.Router; Send defaults to an "ok" result.
type testSender struct {
	router         *session.Router
	getOrCreate    func(ctx context.Context, key string, opts session.AgentOpts) (turn.Session, session.SessionStatus, error)
	send           func(ctx context.Context, key string, sess turn.Session, text string, images []clievent.Attachment, onEvent clievent.EventCallback) (*clievent.SendResult, error)
	notifyIdle     func()
	discardPending func(key string, reason error)
	reset          func(key string, discardOverride bool)
}

var _ turn.Sender = (*testSender)(nil)

// GetOrCreate returns a missing session as a nil interface, like turnSender.
func (s *testSender) GetOrCreate(ctx context.Context, key string, opts sessionview.AgentOpts) (turn.Session, sessionview.SessionStatus, error) {
	if s.getOrCreate != nil {
		return s.getOrCreate(ctx, key, opts)
	}
	if s.router == nil {
		return nil, 0, errors.New("testSender: no router")
	}
	sess, status, err := s.router.GetOrCreate(ctx, key, opts)
	if sess == nil {
		return nil, status, err
	}
	return sess, status, err
}

func (s *testSender) Send(ctx context.Context, key string, sess turn.Session, text string, images []clievent.Attachment, _ turn.SendSpec, onEvent clievent.EventCallback) (*clievent.SendResult, error) {
	if s.send != nil {
		return s.send(ctx, key, sess, text, images, onEvent)
	}
	return &clievent.SendResult{Text: "ok"}, nil
}

func (s *testSender) AfterTurn(string) {}

func (s *testSender) Interrupt(key string) sessionview.InterruptOutcome {
	if s.router == nil {
		return sessionview.InterruptNoSession
	}
	return s.router.InterruptSessionViaControl(key)
}

func (s *testSender) DiscardPending(key string, reason error) {
	if s.discardPending != nil {
		s.discardPending(key, reason)
	} else if s.router != nil {
		s.router.DiscardPassthroughPending(key, reason)
	}
}

func (s *testSender) Reset(key string, discardOverride bool) {
	if s.reset != nil {
		s.reset(key, discardOverride)
	} else if s.router != nil {
		s.router.Reset(key)
	}
}

func (s *testSender) NotifyIdle() {
	if s.notifyIdle != nil {
		s.notifyIdle()
	} else if s.router != nil {
		s.router.NotifyIdle()
	}
}
