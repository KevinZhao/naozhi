package server

// wshub_workflow.go — workflowPushLoop, the per-subscription pump of a
// session's workflow board onto workflow_set / workflow_state frames
// (docs/rfc/workflow-dashboard.md §6.1). completeSubscribe starts it beside
// eventPushLoop. The subscription generation ends it, not the event stream,
// so it keeps pushing while eventPushLoop waits out a CLI restart.

import (
	"cmp"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/wsproto"
)

const (
	// workflowFrameBudget bounds one marshaled workflow_state frame; rows
	// past it are counted in rows_omitted for the client to fetch.
	workflowFrameBudget = 192 << 10
	// workflowQueueDepth: a non-terminal frame is not offered to a send
	// queue holding more than this; the next one supersedes it anyway.
	workflowQueueDepth = 8
	// rowsOmittedSlack is room kept for the rows_omitted field itself.
	rowsOmittedSlack = 32
)

// workflowPace times workflowPushLoop; tests shorten it.
type workflowPace struct {
	retry    time.Duration // after a frame was held back or dropped
	progress time.Duration // least gap between progress-only frames of a task
	alive    time.Duration // re-check of the generation and the board when idle
}

var defaultWorkflowPace = workflowPace{retry: 250 * time.Millisecond, progress: time.Second, alive: 5 * time.Second}

// wfHead is the part of a workflow whose change is sent at once; any other
// change (tokens, tool calls, current, row progress) waits out pace.progress.
type wfHead struct {
	status   workflow.Status
	runID    string
	degraded string
	source   workflow.Source
	counts   workflow.Counts
	phases   int
	capped   bool
}

func headOf(w *workflow.Workflow) wfHead {
	return wfHead{w.Status, w.RunID, w.Degraded, w.Source, w.Counts, len(w.Phases), w.AgentsCapped}
}

// wfSent is what a loop last enqueued of a task.
type wfSent struct {
	version uint64
	at      time.Time
	head    wfHead
}

// workflowPush is one workflowPushLoop's state; only its goroutine uses it.
type workflowPush struct {
	h     *Hub
	c     *wsClient
	key   string
	gen   uint64
	board *session.WorkflowBoard
	wake  <-chan struct{}
	unsub func()
	// set is the sorted task list of the last workflow_set enqueued for
	// board, when setSent.
	set     []string
	setSent bool
	sent    map[string]*wfSent
}

// workflowPushLoop sends the session's board to c: a workflow_set whenever
// its task list changes (an empty board's too), then per task a full frame
// first and deltas after. It follows the key to a session's new board and
// exits once the subscription generation moves on, the client goes or the
// Hub shuts down. It owns one clientWG slot, as eventPushLoop does.
func (h *Hub) workflowPushLoop(c *wsClient, key string, gen uint64, sess *session.ManagedSession) {
	defer func() {
		if r := recover(); r != nil {
			serverMetrics.PanicRecovered()
			slog.Error("panic in ws workflowPushLoop (recovered)", "key", key, "panic", fmt.Sprintf("%v", r))
			slog.Debug("panic in ws workflowPushLoop: stack", "key", key, "stack", string(debug.Stack()))
			c.closeDone()
		}
	}()
	p := &workflowPush{h: h, c: c, key: key, gen: gen, sent: map[string]*wfSent{}}
	// Subscribed before the first Published load: a change after the load
	// wakes the loop again.
	p.watch(sess.WorkflowBoard())
	defer func() { p.unsub() }()
	timer := time.NewTimer(h.workflowPace.alive)
	defer timer.Stop()
	for {
		if !p.current() {
			return
		}
		p.follow()
		next, ok := p.step(time.Now())
		if !ok {
			return
		}
		timer.Reset(min(next, h.workflowPace.alive))
		select {
		case <-p.wake:
		case <-timer.C:
		case <-c.done:
			return
		case <-h.ctx.Done():
			return
		}
	}
}

// watch subscribes to b, which may be nil (a session that has no board yet).
func (p *workflowPush) watch(b *session.WorkflowBoard) {
	p.board = b
	p.wake, p.unsub = b.Subscribe()
}

// current reports whether the subscription the loop serves still stands.
func (p *workflowPush) current() bool {
	gen, ok := p.h.subs.generation(p.c, p.key)
	return ok && gen == p.gen
}

// follow moves to the board of the session now under the key when that is
// another one (/new, eviction and respawn); rename and respawn carry the
// board itself, so they do not get here. The next step starts over with
// the new board's workflow_set.
func (p *workflowPush) follow() {
	cur := p.h.router.SessionFor(p.key)
	if cur == nil {
		return
	}
	if b := cur.WorkflowBoard(); b != p.board {
		p.unsub()
		p.watch(b)
		p.setSent = false
		clear(p.sent)
	}
}

// step enqueues what the board holds that c has not been sent. It returns
// how soon to look again for something held back (pace.alive when nothing
// is), and false once the subscription is gone.
func (p *workflowPush) step(now time.Time) (time.Duration, bool) {
	pace := p.h.workflowPace
	next := pace.alive
	later := func(d time.Duration) { next = min(next, d) }
	pub := p.board.Published()
	var epoch string
	var wfs []*workflow.Workflow
	if pub != nil {
		epoch, wfs = pub.Epoch, pub.Workflows
	}
	ids := make([]string, len(wfs))
	for i, w := range wfs {
		ids[i] = w.TaskID
	}
	sorted := slices.Sorted(slices.Values(ids))
	if !p.setSent || !slices.Equal(sorted, p.set) {
		if !p.current() {
			return 0, false
		}
		if !p.room(true) {
			return pace.retry, true
		}
		data, err := marshalPooled(wsproto.NewWorkflowSet(wsproto.WorkflowSet{Key: p.key, Epoch: epoch, TaskIDs: ids, ServerNow: now.UnixMilli()}))
		if err != nil || !p.c.trySendRaw(data) {
			return pace.retry, true
		}
		p.set, p.setSent = sorted, true
		for id := range p.sent {
			if _, ok := slices.BinarySearch(sorted, id); !ok {
				delete(p.sent, id)
			}
		}
	}
	for _, w := range wfs {
		last := p.sent[w.TaskID]
		if last != nil && last.version >= w.Version {
			continue
		}
		head, terminal := headOf(w), workflow.IsTerminal(w.Status)
		if last != nil && head == last.head && !terminal {
			if wait := last.at.Add(pace.progress).Sub(now); wait > 0 {
				later(wait)
				continue
			}
		}
		if !p.room(terminal) {
			later(pace.retry)
			continue
		}
		if !p.current() {
			return 0, false
		}
		var base uint64
		if last != nil {
			base = last.version
		}
		data, err := p.frame(w, epoch, base, now.UnixMilli())
		if err != nil {
			// Deterministic for this publication; the next change retries.
			slog.Warn("workflow frame marshal failed", "key", p.key, "task", w.TaskID, "err", err)
			continue
		}
		if !p.c.trySendRaw(data) {
			later(pace.retry)
			continue
		}
		p.sent[w.TaskID] = &wfSent{version: w.Version, at: now, head: head}
	}
	return next, true
}

// room is the send-queue depth gate: a frame is not offered to a queue too
// deep to take it, so a backlog never turns retries into drops (each one
// counts toward wsDropThreshold). Terminal frames and workflow_set get half
// the queue, any other frame workflowQueueDepth.
func (p *workflowPush) room(terminal bool) bool {
	if terminal {
		return len(p.c.send) < cap(p.c.send)/2
	}
	return len(p.c.send) <= workflowQueueDepth
}

// frame marshals w as a full frame (base 0) or as the delta after base. A
// delta over workflowFrameBudget carries the changed rows that fit, failed
// first and done last, and counts the others in rows_omitted.
func (p *workflowPush) frame(w *workflow.Workflow, epoch string, base uint64, now int64) ([]byte, error) {
	f := wsproto.WorkflowState{Key: p.key, TaskID: w.TaskID, Epoch: epoch, Version: w.Version, BaseVersion: base, Full: base == 0, ServerNow: now}
	var rows []workflow.Agent
	if base > 0 {
		rows = workflow.RowsAfter(w.Agents, base)
	}
	f.Workflow = w.Wire(rows)
	data, err := marshalPooled(wsproto.NewWorkflowState(f))
	if err != nil || len(data) <= workflowFrameBudget {
		return data, err
	}
	f.Workflow = w.Wire(nil)
	head, err := marshalPooled(wsproto.NewWorkflowState(f))
	if err != nil {
		return nil, err
	}
	room := workflowFrameBudget - len(head) - rowsOmittedSlack
	order := slices.Clone(rows)
	slices.SortStableFunc(order, func(a, b workflow.Agent) int { return cmp.Compare(rowRank(a.State), rowRank(b.State)) })
	keep := make([]workflow.Agent, 0, len(order))
	for _, a := range order {
		// b ends in the encoder's newline, which stands for the comma.
		b, err := marshalPooled(a)
		if err != nil {
			return nil, err
		}
		if len(b) <= room {
			keep = append(keep, a)
			room -= len(b)
		}
	}
	slices.SortFunc(keep, func(a, b workflow.Agent) int { return cmp.Compare(a.Index, b.Index) })
	f.RowsOmitted = len(rows) - len(keep)
	f.Workflow = w.Wire(keep)
	return marshalPooled(wsproto.NewWorkflowState(f))
}

// rowRank orders rows for a frame over budget: failed, running, queued,
// the other settled states, done.
func rowRank(s workflow.AgentState) int {
	switch s {
	case workflow.AgentFailed:
		return 0
	case workflow.AgentRunning:
		return 1
	case workflow.AgentQueued:
		return 2
	case workflow.AgentDone:
		return 4
	}
	return 3
}
