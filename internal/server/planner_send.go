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
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// errPlannerNamespace answers a dashboard or relayed send to a planner: key.
var errPlannerNamespace = sendUsageError("Planner 的消息请从 Planner 页面发送")

var (
	errPlannerBusy     = errors.New("Planner 正忙，消息未送达，请稍后重试")
	errPlannerClosing  = errors.New("服务正在关闭，消息未送达")
	errPlannerDropped  = errors.New("消息未被处理")
	errPlannerNoEngine = errors.New("planner turn entry not wired")
)

// SubmitPlanner runs text as a turn on key, spawning it with opts; done (nil
// for none) gets the reply text or the turn's error. It is the agent's
// Turns.SubmitPlanner.
func (s *Server) SubmitPlanner(key, text string, opts session.AgentOpts, done func(reply string, err error)) error {
	if s.hub == nil || s.hub.engine == nil {
		return errPlannerNoEngine
	}
	return s.hub.engine.submitPlanner(key, text, opts, done)
}

// PlannerReceipt shows text to key's subscribers as a system event. It is
// not persisted and never reaches the CLI: the receipt is the gateway's, not
// part of the Planner's conversation.
func (s *Server) PlannerReceipt(key, text string) {
	if s.hub != nil {
		s.hub.bcast.broadcastSessionSystemEvent(key, text)
	}
}

func (e *sendEngine) submitPlanner(key, text string, opts session.AgentOpts, done func(string, error)) error {
	o := &plannerOrigin{key: key, opts: opts, notify: e.notify, done: done}
	switch e.turns.Submit(e.ctx, turn.Request{Key: key, Text: text, Origin: o}, dashAdmission{track: e.TrackSend, ctx: e.ctx}) {
	case turn.AckOwner, turn.AckQueued, turn.AckDetached:
		return nil
	case turn.AckDropped:
		return errPlannerBusy
	default:
		return errPlannerClosing
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
