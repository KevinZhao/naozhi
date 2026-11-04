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
	"slices"
	"sync"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// errPlannerNamespace answers a dashboard or relayed send to a planner: key.
var errPlannerNamespace = sendUsageError("Planner 的消息请从 Planner 页面发送")

// errTaskNamespace answers a send creating a task: session: only a dispatch
// block does.
var errTaskNamespace = sendUsageError("task: 会话只能由 Planner 派出")

// errScoutNamespace answers a send to a scout: key: only a scout run talks
// to one.
var errScoutNamespace = sendUsageError("巡检会话不接收消息")

var (
	errPlannerBusy     = errors.New("服务忙：Planner 消息未送达，请稍后重试")
	errPlannerClosing  = errors.New("服务正在关闭，消息未送达")
	errPlannerDropped  = errors.New("消息未被处理")
	errPlannerNoEngine = errors.New("planner turn entry not wired")
)

// SubmitPlanner runs text from src as a turn on key, spawning it with opts;
// done (nil for none) gets the reply text or the turn's error. queued is
// text waiting behind a running turn. It is the agent's Turns.SubmitPlanner,
// and sends the first message of a session the Planner dispatched (task:,
// src ""), which also spawns with options the gateway set.
func (s *Server) SubmitPlanner(key string, src agent.Source, text string, opts session.AgentOpts, done func(reply string, err error)) (queued bool, err error) {
	if s.hub == nil || s.hub.engine == nil {
		return false, errPlannerNoEngine
	}
	return s.hub.engine.submitPlanner(key, src, text, opts, done)
}

// PlannerTurnSources is the sources of the requests key's turn in flight
// took, sorted; none between turns. It is the agent's
// Turns.PlannerTurnSources.
func (s *Server) PlannerTurnSources(key string) []agent.Source {
	if s.hub == nil || s.hub.engine == nil {
		return nil
	}
	return s.hub.engine.plannerTurns.sources(key)
}

// plannerTurns is the sources of the requests each key's turn in flight
// took. A turn begins every request it takes before it finishes any (the
// turn package's runTurn and deliver), and the next turn begins after, so
// the first finish ends the turn.
type plannerTurns struct {
	mu   sync.Mutex
	srcs map[string][]agent.Source
}

func (t *plannerTurns) begin(key string, src agent.Source) {
	if src == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srcs == nil {
		t.srcs = map[string][]agent.Source{}
	}
	if !slices.Contains(t.srcs[key], src) {
		t.srcs[key] = append(t.srcs[key], src)
	}
}

func (t *plannerTurns) finish(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.srcs, key)
}

func (t *plannerTurns) sources(key string) []agent.Source {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := slices.Clone(t.srcs[key])
	slices.Sort(out)
	return out
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
// drops it, so the agent journals its notes too (agent.Agent.Note). With no
// session for key it falls back to a broadcast.
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

func (e *sendEngine) submitPlanner(key string, src agent.Source, text string, opts session.AgentOpts, done func(string, error)) (bool, error) {
	o := &plannerOrigin{key: key, src: src, turns: &e.plannerTurns, opts: opts, notify: e.notify, done: done}
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
	key string
	// src is what the request came from, recorded in turns while its turn
	// is in flight.
	src    agent.Source
	turns  *plannerTurns
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
	o.turns.begin(o.key, o.src)
	return o
}

func (o *plannerOrigin) Finish(_ context.Context, out turn.Outcome) {
	o.turns.finish(o.key)
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
