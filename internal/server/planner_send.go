// planner_send.go — the turn entry of the global Planner (internal/planner/agent).
// Its messages skip prepareSend, which refuses the planner: namespace, so
// planner:main only ever spawns with the launch spec the agent hands in,
// never as an ordinary dashboard session.
package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// errPlannerNamespace answers a dashboard or relayed send to a planner: key.
var errPlannerNamespace = sendUsageError("Planner 的消息请从 Planner 页面发送")

var (
	errPlannerBusy     = errors.New("服务忙：Planner 消息未送达，请稍后重试")
	errPlannerClosing  = errors.New("服务正在关闭，消息未送达")
	errPlannerDropped  = errors.New("消息未被处理")
	errPlannerNoEngine = errors.New("planner turn entry not wired")
)

// SubmitPlanner runs text as a turn on key, spawning it with opts; done (nil
// for none) gets the reply text or the turn's error. queued is text waiting
// behind a running turn. It is the agent's Turns.SubmitPlanner.
func (s *Server) SubmitPlanner(key, text string, opts session.AgentOpts, done func(reply string, err error)) (queued bool, err error) {
	if s.hub == nil || s.hub.engine == nil {
		return false, errPlannerNoEngine
	}
	return s.hub.engine.submitPlanner(key, text, opts, done)
}

// DropPlannerQueue discards the messages queued for key's next turn; their
// origins are told DropRemoved. The running turn, if any, is left alone.
func (s *Server) DropPlannerQueue(key string) {
	if s.hub != nil && s.hub.engine != nil {
		s.hub.engine.turns.Retire(s.hub.engine.ctx, key)
	}
}

// PlannerReceipt shows text to key's subscribers as a system event. It is
// not persisted and never reaches the CLI: the receipt is the gateway's, not
// part of the Planner's conversation.
func (s *Server) PlannerReceipt(key, text string) {
	if s.hub != nil {
		s.hub.bcast.broadcastSessionSystemEvent(key, text)
	}
}

// PlannerNote writes text into key's conversation history as a system event,
// so a page opened later still shows it; subscribers see it at once. Unlike
// PlannerReceipt it is part of the conversation the dashboard replays, though
// it still never reaches the CLI. The history is in memory: a gateway restart
// drops it. With no session for key it falls back to a broadcast.
func (s *Server) PlannerNote(key, text string) {
	text = osutil.SanitizeForLog(text, plannerNoteBytes)
	if s.router != nil {
		if sess := s.router.SessionFor(key); sess != nil {
			sess.LogSystemEvent(text)
			return
		}
	}
	s.PlannerReceipt(key, text)
}

// plannerNoteBytes caps a note; a promise's text is at most 500 runes.
const plannerNoteBytes = 2048

func (e *sendEngine) submitPlanner(key, text string, opts session.AgentOpts, done func(string, error)) (bool, error) {
	o := &plannerOrigin{key: key, opts: opts, notify: e.notify, done: done}
	switch e.turns.Submit(e.ctx, turn.Request{Key: key, Text: text, Origin: o}, dashAdmission{track: e.TrackSend, ctx: e.ctx}) {
	case turn.AckOwner, turn.AckDetached:
		return false, nil
	case turn.AckQueued:
		return true, nil
	case turn.AckDropped:
		return false, errPlannerBusy
	default:
		return false, errPlannerClosing
	}
}

// plannerOrigin is one Planner turn request. Each has its own sink so a
// request waiting for its reply (the handoff) is finished on its own even
// when it merges with a user message. A failure is broadcast like an HTTP
// send's.
type plannerOrigin struct {
	key    string
	opts   session.AgentOpts
	notify sendNotifier
	done   func(string, error)
}

func (o *plannerOrigin) Sink() string                         { return fmt.Sprintf("planner:%p", o) }
func (*plannerOrigin) Admitted(context.Context, turn.Ack)     {}
func (o *plannerOrigin) SessionOpts(string) session.AgentOpts { return o.opts }
func (*plannerOrigin) BeforeSession(context.Context)          {}
func (*plannerOrigin) Blocking() bool                         { return false }
func (*plannerOrigin) SessionReady(context.Context, session.SessionStatus) clievent.EventCallback {
	return nil
}

func (o *plannerOrigin) Begin(_ context.Context, t turn.TurnInfo) turn.Delivery {
	if t.Role == turn.RoleObserver {
		return nil
	}
	return o
}

func (o *plannerOrigin) Finish(_ context.Context, out turn.Outcome) {
	msg, failed, err := dashOrigin{key: o.key}.failure(out)
	if failed && !informationalSendErr(err) {
		o.notify.broadcastSendError(o.key, msg)
	}
	if o.done == nil {
		return
	}
	switch {
	case failed:
		o.done("", cmp.Or(err, errors.New(msg)))
	case out.Result == nil:
		o.done("", nil)
	default:
		o.done(out.Result.Text, nil)
	}
}

func (o *plannerOrigin) Dropped(context.Context, turn.DropReason) {
	if o.done != nil {
		o.done("", errPlannerDropped)
	}
}
