// planner_send.go — the turn entry of the global Planner (internal/planner/agent).
// Its messages skip prepareSend, which refuses the planner: namespace, so
// planner:main only ever spawns with the launch spec the agent hands in,
// never as an ordinary dashboard session.
package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/datadir"
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

// PlannerTurnSources is the sources of key's requests in flight, sorted;
// none between turns. It is the agent's Turns.PlannerTurnSources.
func (s *Server) PlannerTurnSources(key string) []agent.Source {
	if s.hub == nil || s.hub.engine == nil {
		return nil
	}
	return s.hub.engine.plannerTurns.sources(key)
}

// PlannerResult tells the turn ledger a result of key's arrived and was
// judged: it answers the oldest request carried over from the last run, if
// any. The router's result sink calls it after the action runner.
func (s *Server) PlannerResult(key string) {
	if s.hub != nil && s.hub.engine != nil {
		s.hub.engine.plannerTurns.resulted(key)
	}
}

// plannerTurnMaxAge bounds a request carried over from the last run: no
// turn outlives the watchdog's total timeout.
const plannerTurnMaxAge = cliinfo.DefaultTotalTimeout

// plannerTurns is the sources of the requests in flight, by which a turn's
// result is judged (agent.Agent.TurnSources). A request counts from its
// Begin to its own Finish. Requests on one key may run side by side
// (passthrough's detached turns, a PriorityNow send) and the CLI may answer
// them in one result, so a result is judged by all of them: never by fewer
// sources than its own. The list is kept on disk (path) as well: a
// shim-held turn finishes on the CLI the next run reattaches, so the
// requests in flight when a run ended are carried over (restored), each
// until a result answers it or it is older than any turn runs.
type plannerTurns struct {
	path string
	// ctx is the engine's. Once it is done (shutdown) a finish no longer
	// reaches the file: the turns a shim carries over stay on it.
	ctx context.Context

	mu       sync.Mutex
	next     uint64
	live     map[uint64]plannerTurn
	restored []plannerTurn
}

type plannerTurn struct {
	Key   string       `json:"key"`
	Src   agent.Source `json:"source"`
	Began time.Time    `json:"began"`
	// After is the session's shim watermark as the request began
	// (cli.TurnWatermark.String; "" when there was none): a result the
	// next run finds in the shim's backlog past it is this turn's.
	After string `json:"after,omitempty"`
}

type plannerTurnsFile struct {
	V     int           `json:"v"`
	Turns []plannerTurn `json:"turns"`
}

// load sets the file and the engine's ctx and carries over the requests
// the file holds.
func (t *plannerTurns) load(ctx context.Context, path string) {
	t.path, t.ctx = path, ctx
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var f plannerTurnsFile
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	if err != nil {
		slog.Warn("planner turns: ledger unreadable; no request carried over", "path", path, "err", err)
		return
	}
	for _, e := range f.Turns {
		if e.Src != "" && time.Since(e.Began) < plannerTurnMaxAge {
			t.restored = append(t.restored, e)
		}
	}
	if len(t.restored) > 0 {
		slog.Info("planner turns: requests carried over from the last run", "count", len(t.restored))
	}
}

// begin records a request from src on key, at key's shim watermark after,
// and returns its ID; a request with no source (a task: session's first
// message) is not recorded, 0.
func (t *plannerTurns) begin(key string, src agent.Source, after string) uint64 {
	if src == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live == nil {
		t.live = map[uint64]plannerTurn{}
	}
	t.next++
	t.live[t.next] = plannerTurn{Key: key, Src: src, Began: time.Now(), After: after}
	t.saveLocked()
	return t.next
}

func (t *plannerTurns) finish(id uint64) {
	if id == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.live, id)
	t.saveLocked()
}

// resulted drops key's oldest carried-over request.
func (t *plannerTurns) resulted(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i := slices.IndexFunc(t.restored, func(e plannerTurn) bool { return e.Key == key })
	if i < 0 {
		return
	}
	t.restored = slices.Delete(t.restored, i, i+1)
	t.saveLocked()
}

// redeliver offers deliver each key's requests carried over from the last
// run, oldest first, until one is answered: a turn that ended while naozhi
// was down left its result in the shim's backlog, which no read loop hands
// on. A request with no watermark is skipped, since the backlog may end in
// a result the last run delivered. The delivered result reaches the sink,
// whose PlannerResult lets go of the request.
func (t *plannerTurns) redeliver(deliver func(key string, after cli.TurnWatermark) bool) {
	t.mu.Lock()
	carried := slices.Clone(t.restored)
	t.mu.Unlock()
	answered := map[string]bool{}
	for _, e := range carried {
		w, ok := cli.ParseTurnWatermark(e.After)
		if ok && !answered[e.Key] {
			answered[e.Key] = deliver(e.Key, w)
		}
	}
}

func (t *plannerTurns) sources(key string) []agent.Source {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []agent.Source
	for _, e := range slices.Concat(t.restored, slices.Collect(maps.Values(t.live))) {
		if e.Key == key {
			out = append(out, e.Src)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// saveLocked writes the requests in flight; it fails open to a warning,
// the list in memory still holding.
func (t *plannerTurns) saveLocked() {
	if t.path == "" || (t.ctx != nil && t.ctx.Err() != nil) {
		return
	}
	f := plannerTurnsFile{V: 1, Turns: slices.Clone(t.restored)}
	for _, id := range slices.Sorted(maps.Keys(t.live)) {
		f.Turns = append(f.Turns, t.live[id])
	}
	data, err := json.Marshal(f)
	if err == nil {
		err = datadir.EnsureDir(filepath.Dir(t.path))
	}
	if err == nil {
		err = osutil.WriteFileAtomic(t.path, data, 0o600)
	}
	if err != nil {
		slog.Warn("planner turns: save the ledger", "path", t.path, "err", err)
	}
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

// plannerMark is key's shim watermark now, encoded; "" when it has none.
func (e *sendEngine) plannerMark(key string) string {
	if e.router == nil {
		return ""
	}
	if sess := e.router.SessionFor(key); sess != nil {
		if w, ok := sess.TurnWatermark(); ok {
			return w.String()
		}
	}
	return ""
}

// deliverReplayed hands the result sink key's reply the reconnect found in
// the shim's backlog past after.
func (e *sendEngine) deliverReplayed(key string, after cli.TurnWatermark) bool {
	if e.router == nil {
		return false
	}
	sess := e.router.SessionFor(key)
	return sess != nil && sess.DeliverReplayedResult(after, true)
}

func (e *sendEngine) submitPlanner(key string, src agent.Source, text string, opts session.AgentOpts, done func(string, error)) (bool, error) {
	o := &plannerOrigin{key: key, src: src, turns: &e.plannerTurns, mark: e.plannerMark, opts: opts, notify: e.notify, done: done}
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
	// src is what the request came from, recorded in turns from its Begin
	// to its Finish under id.
	src   agent.Source
	id    uint64
	turns *plannerTurns
	// mark is key's shim watermark now, encoded; nil records none.
	mark   func(key string) string
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
	if o.id == 0 && o.src != "" {
		after := ""
		if o.mark != nil {
			after = o.mark(o.key)
		}
		o.id = o.turns.begin(o.key, o.src, after)
	}
	return o
}

func (o *plannerOrigin) Finish(_ context.Context, out turn.Outcome) {
	o.turns.finish(o.id)
	o.id = 0
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
