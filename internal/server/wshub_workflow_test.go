package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const wfKey = "test:d:u:general"

// wfMsg is a workflow_set or workflow_state frame as the dashboard reads it.
type wfMsg struct {
	Type        string            `json:"type"`
	Key         string            `json:"key"`
	TaskID      string            `json:"task_id"`
	Epoch       string            `json:"epoch"`
	Version     uint64            `json:"version"`
	BaseVersion uint64            `json:"base_version"`
	Full        bool              `json:"full"`
	ServerNow   int64             `json:"server_now"`
	RowsOmitted int               `json:"rows_omitted"`
	TaskIDs     []string          `json:"task_ids"`
	Workflow    workflow.WireView `json:"workflow"`
	size        int
}

// wfSource stands in for a CLI process's workflow Tracker: Publish hands
// the board it is bound to a newer Set.
type wfSource struct {
	sess *session.ManagedSession
	mu   sync.Mutex
	set  *workflow.Set
	cb   func()
}

// bindSource binds a new source to sess's board, as a new CLI process would.
func bindSource(sess *session.ManagedSession) *wfSource {
	src := &wfSource{sess: sess, set: &workflow.Set{}}
	sess.BindWorkflowsForTest(src)
	return src
}

// Publish makes wfs (taken over) the source's current Set and wakes the board.
func (f *wfSource) Publish(wfs ...*workflow.Workflow) {
	f.mu.Lock()
	f.set = &workflow.Set{Workflows: wfs, Version: f.set.Version + 1}
	cb := f.cb
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// End settles the source's workflows as its process ending with end does.
func (f *wfSource) End(end cli.ProcessEnd) { f.sess.EndWorkflowsForTest(f, end) }

func (f *wfSource) Workflows() *workflow.Set {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.set
}

func (f *wfSource) SetOnWorkflowChange(fn func()) {
	f.mu.Lock()
	f.cb = fn
	f.mu.Unlock()
}

func (f *wfSource) KnowWorkflowTasks([]string) {}

func (f *wfSource) ApplyWorkflowResult(*workflow.ResultFile) bool { return false }

// newWorkflowHub is a test Hub whose workflow loops run on short timers.
func newWorkflowHub(t *testing.T) (*Hub, *session.Router) {
	t.Helper()
	hub, router := newTestHub(t, "")
	hub.workflowPace = workflowPace{retry: 20 * time.Millisecond, progress: 300 * time.Millisecond, alive: 50 * time.Millisecond}
	t.Cleanup(hub.Shutdown)
	return hub, router
}

// startWorkflowLoop subscribes c to wfKey and runs its workflowPushLoop on
// sess; the returned channel closes when the loop returns.
func startWorkflowLoop(t *testing.T, hub *Hub, c *wsClient, sess *session.ManagedSession) <-chan struct{} {
	t.Helper()
	hub.register(c)
	gen := subscribeTest(hub, c, wfKey, func() {})
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.workflowPushLoop(c, wfKey, gen, sess)
	}()
	t.Cleanup(func() {
		c.closeDone()
		<-done
	})
	return done
}

// nextWF returns c's next workflow frame, skipping any other.
func nextWF(t *testing.T, c *wsClient) wfMsg {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case data := <-c.send:
			var m wfMsg
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("unmarshal %s: %v", data, err)
			}
			if isWorkflowFrame(m.Type) {
				m.size = len(data)
				return m
			}
		case <-deadline:
			t.Fatal("no workflow frame")
		}
	}
}

// noWF fails if a workflow frame reaches c within d.
func noWF(t *testing.T, c *wsClient, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case data := <-c.send:
			if strings.Contains(string(data), `"type":"workflow_`) {
				t.Fatalf("unexpected workflow frame %s", data)
			}
		case <-deadline:
			return
		}
	}
}

// fill enqueues n placeholder frames.
func fill(c *wsClient, n int) {
	for range n {
		c.send <- []byte(`{"type":"pong"}`)
	}
}

// drain empties c's send queue of placeholders.
func drain(t *testing.T, c *wsClient, n int) {
	t.Helper()
	for range n {
		if data := <-c.send; string(data) != `{"type":"pong"}` {
			t.Fatalf("drained %s, want a placeholder", data)
		}
	}
}

func row(i int, st workflow.AgentState) workflow.Agent {
	return workflow.Agent{Index: i, Label: fmt.Sprintf("agent %d", i), State: st}
}

// wfOf is a live workflow as a Tracker would publish it.
func wfOf(id string, st workflow.Status, rows ...workflow.Agent) *workflow.Workflow {
	w := &workflow.Workflow{TaskID: id, Name: "probe", Status: st, Source: workflow.SourceStream,
		StartedAt: 1000, LastObservedAt: time.Now().UnixMilli(), Agents: rows, Phases: []workflow.Phase{{Index: 1, Title: "Ask"}}}
	for _, a := range rows {
		w.Counts.Total++
		switch a.State {
		case workflow.AgentDone:
			w.Counts.Done++
		case workflow.AgentRunning:
			w.Counts.Running++
		case workflow.AgentQueued:
			w.Counts.Queued++
		}
	}
	if workflow.IsTerminal(st) {
		w.EndedAt = 2000
	}
	return w
}

func rowIndexes(m wfMsg) []int {
	var out []int
	for _, a := range m.Workflow.Agents {
		out = append(out, a.Index)
	}
	return out
}

// The loop's opening: the board's task list, then per task a full frame
// with the header and phases but no rows; after that a delta with only the
// rows changed since.
func TestWorkflowPush_SetThenFullThenDelta(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning), row(1, workflow.AgentQueued)))
	c := newTestWSClient()
	startWorkflowLoop(t, hub, c, sess)

	set := nextWF(t, c)
	epoch := sess.WorkflowBoard().Published().Epoch
	if set.Type != "workflow_set" || set.Key != wfKey || set.Epoch != epoch || !slices.Equal(set.TaskIDs, []string{"wtask0001"}) || set.ServerNow == 0 {
		t.Fatalf("first frame = %+v, want the board's workflow_set", set)
	}
	full := nextWF(t, c)
	if full.Type != "workflow_state" || !full.Full || full.BaseVersion != 0 || full.Epoch != epoch || len(full.Workflow.Agents) != 0 ||
		full.Workflow.Name != "probe" || len(full.Workflow.Phases) != 1 || full.Version == 0 || full.ServerNow == 0 {
		t.Fatalf("second frame = %+v, want a header-only full frame", full)
	}

	src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentDone), row(1, workflow.AgentQueued)))
	d := nextWF(t, c)
	if d.Full || d.BaseVersion != full.Version || d.Version <= full.Version || !slices.Equal(rowIndexes(d), []int{0}) || d.Workflow.Agents[0].State != workflow.AgentDone {
		t.Fatalf("delta = %+v, want row 0 only, based on %d", d, full.Version)
	}
	noWF(t, c, 150*time.Millisecond)
}

// An empty board, and a session that has none yet, still open with a
// workflow_set: the dashboard learns it holds nothing for the key.
func TestWorkflowPush_EmptyBoardSendsEmptySet(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	c := newTestWSClient()
	startWorkflowLoop(t, hub, c, sess)
	if m := nextWF(t, c); m.Type != "workflow_set" || m.TaskIDs == nil || len(m.TaskIDs) != 0 || m.Epoch != "" {
		t.Fatalf("frame = %+v, want an empty workflow_set", m)
	}

	bindSource(sess)
	c2 := newTestWSClient()
	hub2, _ := newWorkflowHub(t)
	hub2.router = router
	startWorkflowLoop(t, hub2, c2, sess)
	if m := nextWF(t, c2); m.Type != "workflow_set" || len(m.TaskIDs) != 0 || m.Epoch != sess.WorkflowBoard().Published().Epoch {
		t.Fatalf("frame = %+v, want the empty board's workflow_set", m)
	}
	noWF(t, c2, 150*time.Millisecond)
}

// newPush is the state of a loop for c on sess's board, subscribed as the
// loop would be, for tests that drive step by hand.
func newPush(t *testing.T, hub *Hub, c *wsClient, sess *session.ManagedSession) *workflowPush {
	t.Helper()
	hub.register(c)
	p := &workflowPush{h: hub, c: c, key: wfKey, gen: subscribeTest(hub, c, wfKey, func() {}), sent: map[string]*wfSent{}}
	p.watch(sess.WorkflowBoard())
	t.Cleanup(func() { p.unsub() })
	return p
}

// stepOK runs one step at now and fails if the subscription looks gone.
func stepOK(t *testing.T, p *workflowPush, now time.Time) time.Duration {
	t.Helper()
	next, ok := p.step(now)
	if !ok {
		t.Fatal("step: the subscription is gone")
	}
	return next
}

// A frame the full queue dropped is not recorded as sent: the retry's
// delta is based on the last frame delivered, so it carries the rows of
// both publications.
func TestWorkflowPush_DroppedFrameResent(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning), row(1, workflow.AgentRunning), row(2, workflow.AgentRunning)))
	c := newTestWSClient()
	c.send = make(chan []byte, 4)
	p := newPush(t, hub, c, sess)
	t0 := time.Now()
	stepOK(t, p, t0)
	nextWF(t, c)
	full := nextWF(t, c)

	fill(c, cap(c.send))
	src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentDone), row(1, workflow.AgentRunning), row(2, workflow.AgentRunning)))
	if next := stepOK(t, p, t0); next != hub.workflowPace.retry || c.dropped.Load() != 1 {
		t.Fatalf("after the drop: next %v, drops %d; want a retry armed and one drop", next, c.dropped.Load())
	}
	src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentDone), row(1, workflow.AgentDone), row(2, workflow.AgentRunning)))
	drain(t, c, cap(c.send))
	stepOK(t, p, t0.Add(time.Second))
	d := nextWF(t, c)
	if d.BaseVersion != full.Version || !slices.Equal(rowIndexes(d), []int{0, 1}) {
		t.Fatalf("delta after the drop = %+v, want rows 0 and 1 based on %d", d, full.Version)
	}
}

// Progress alone goes out at most once per pace.progress, carrying the
// latest state; a status change goes out at once.
func TestWorkflowPush_ProgressPaced(t *testing.T) {
	hub, router := newWorkflowHub(t)
	hub.workflowPace.alive = time.Hour
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning)))
	c := newTestWSClient()
	p := newPush(t, hub, c, sess)
	t0 := time.Now()
	stepOK(t, p, t0)
	nextWF(t, c)
	nextWF(t, c)

	pace := hub.workflowPace.progress
	for i := 1; i <= 5; i++ {
		w := wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning))
		w.Tokens = int64(i)
		src.Publish(w)
		if next := stepOK(t, p, t0.Add(time.Duration(i)*10*time.Millisecond)); next != pace-time.Duration(i)*10*time.Millisecond {
			t.Fatalf("progress %d: next %v, want the rest of the %v interval", i, next, pace)
		}
	}
	if n := len(c.send); n != 0 {
		t.Fatalf("%d frames inside the interval", n)
	}
	stepOK(t, p, t0.Add(pace))
	if m := nextWF(t, c); m.Workflow.Tokens != 5 {
		t.Fatalf("paced frame = %+v, want the latest tokens", m)
	}
	src.Publish(wfOf("wtask0001", workflow.StatusCompleted, row(0, workflow.AgentDone)))
	stepOK(t, p, t0.Add(pace+time.Millisecond))
	if m := nextWF(t, c); m.Workflow.Status != workflow.StatusCompleted {
		t.Fatalf("terminal frame = %+v, want it at once", m)
	}
	w := wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning))
	w.Counts.Done = 1
	src.Publish(w)
	stepOK(t, p, t0.Add(pace+2*time.Millisecond))
	if m := nextWF(t, c); m.Workflow.Status != workflow.StatusRunning {
		t.Fatalf("status change frame = %+v, want it at once", m)
	}
}

// Past workflowQueueDepth queued frames a running workflow's frame is not
// offered at all, so the backlog costs no drops; it goes once the queue
// drains.
func TestWorkflowPush_DeepQueueHoldsRunning(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning)))
	c := newTestWSClient()
	p := newPush(t, hub, c, sess)
	t0 := time.Now()
	stepOK(t, p, t0)
	nextWF(t, c)
	nextWF(t, c)

	fill(c, workflowQueueDepth+1)
	src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentDone)))
	for i := range 20 {
		if next := stepOK(t, p, t0.Add(time.Duration(i)*time.Second)); next != hub.workflowPace.retry {
			t.Fatalf("held frame: next %v, want a retry", next)
		}
	}
	if n := len(c.send); n != workflowQueueDepth+1 || c.dropped.Load() != 0 {
		t.Fatalf("queue %d, drops %d: the frame was offered to a deep queue", n, c.dropped.Load())
	}
	drain(t, c, 1)
	stepOK(t, p, t0.Add(time.Minute))
	drain(t, c, workflowQueueDepth)
	if m := nextWF(t, c); m.Workflow.Counts.Done != 1 {
		t.Fatalf("frame = %+v, want the held change", m)
	}
}

// Three workflows ending together on a full queue: no frame is offered
// while it is half full, so no retry counts a drop and the client is not
// cut off; all three end frames go once it drains.
func TestWorkflowPush_FullQueueTerminal(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	ids := []string{"wtask0001", "wtask0002", "wtask0003"}
	var wfs []*workflow.Workflow
	for _, id := range ids {
		wfs = append(wfs, wfOf(id, workflow.StatusRunning, row(0, workflow.AgentRunning)))
	}
	src.Publish(wfs...)
	c := newTestWSClient()
	p := newPush(t, hub, c, sess)
	t0 := time.Now()
	stepOK(t, p, t0)
	for range 4 {
		nextWF(t, c)
	}

	fill(c, cap(c.send))
	wfs = nil
	for _, id := range ids {
		wfs = append(wfs, wfOf(id, workflow.StatusCompleted, row(0, workflow.AgentDone)))
	}
	src.Publish(wfs...)
	for i := range 4 * wsDropThreshold {
		stepOK(t, p, t0.Add(time.Duration(i)*hub.workflowPace.retry))
	}
	select {
	case <-c.done:
		t.Fatal("the client was closed")
	default:
	}
	if n := c.dropped.Load(); n != 0 {
		t.Fatalf("%d drops while the queue was full", n)
	}
	drain(t, c, cap(c.send)/2)
	stepOK(t, p, t0.Add(time.Minute))
	if n, d := len(c.send), c.dropped.Load(); n != cap(c.send)/2 || d != 0 {
		t.Fatalf("at half the queue: %d queued, %d drops; want nothing offered", n, d)
	}
	drain(t, c, cap(c.send)/2)
	stepOK(t, p, t0.Add(2*time.Minute))
	var got []string
	for range ids {
		m := nextWF(t, c)
		if m.Workflow.Status != workflow.StatusCompleted {
			t.Fatalf("frame = %+v, want an end frame", m)
		}
		got = append(got, m.TaskID)
	}
	slices.Sort(got)
	if !slices.Equal(got, ids) {
		t.Fatalf("end frames for %v, want %v", got, ids)
	}
}

// A task the board drops leaves the next workflow_set; one that comes back
// opens with a full frame again.
func TestWorkflowPush_DroppedTaskLeavesSet(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	var wfs []*workflow.Workflow
	for i := range 6 {
		w := wfOf(fmt.Sprintf("wtask000%d", i), workflow.StatusCompleted)
		w.EndedAt = int64(2000 + i)
		wfs = append(wfs, w)
	}
	src.Publish(wfs...)
	c := newTestWSClient()
	startWorkflowLoop(t, hub, c, sess)
	if m := nextWF(t, c); len(m.TaskIDs) != 6 {
		t.Fatalf("set = %+v, want 6 tasks", m)
	}
	for range 6 {
		nextWF(t, c)
	}

	// A new process takes over: the six are the board's own now, one too
	// many for its terminal bound, and the earliest ended goes.
	src2 := bindSource(sess)
	set := nextWF(t, c)
	if set.Type != "workflow_set" || len(set.TaskIDs) != 5 || slices.Contains(set.TaskIDs, "wtask0000") {
		t.Fatalf("frame = %+v, want a set without wtask0000", set)
	}
	src2.Publish(wfOf("wtask0000", workflow.StatusRunning, row(0, workflow.AgentRunning)))
	if m := nextWF(t, c); m.Type != "workflow_set" || !slices.Contains(m.TaskIDs, "wtask0000") {
		t.Fatalf("frame = %+v, want the set with wtask0000 back", m)
	}
	for {
		m := nextWF(t, c)
		if m.TaskID != "wtask0000" {
			continue
		}
		if !m.Full {
			t.Fatalf("frame = %+v, want a full frame for the returning task", m)
		}
		break
	}
}

// bigRows is n rows of about 300 bytes each.
func bigRows(n int, st workflow.AgentState) []workflow.Agent {
	rows := make([]workflow.Agent, n)
	for i := range rows {
		rows[i] = row(i, st)
		rows[i].LastToolSummary = strings.Repeat("s", 200)
	}
	return rows
}

// A delta over the frame budget carries the changed rows that fit, failed
// first, and counts the rest; every frame stays within the budget.
func TestWorkflowPush_FrameBudget(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	src.Publish(wfOf("wtask0001", workflow.StatusRunning))
	c := newTestWSClient()
	startWorkflowLoop(t, hub, c, sess)
	nextWF(t, c)
	nextWF(t, c)

	src.Publish(wfOf("wtask0001", workflow.StatusRunning, bigRows(workflow.MaxAgents, workflow.AgentQueued)...))
	m := nextWF(t, c)
	if m.size > workflowFrameBudget || m.RowsOmitted == 0 || len(m.Workflow.Agents)+m.RowsOmitted != workflow.MaxAgents {
		t.Fatalf("2000 queued rows: %d bytes, %d rows, %d omitted", m.size, len(m.Workflow.Agents), m.RowsOmitted)
	}
	if !slices.IsSortedFunc(m.Workflow.Agents, func(a, b workflow.Agent) int { return a.Index - b.Index }) {
		t.Error("rows are not in index order")
	}

	rows := bigRows(workflow.MaxAgents, workflow.AgentStopped)
	for _, i := range []int{1990, 1995, 1999} {
		rows[i].State = workflow.AgentFailed
	}
	src.Publish(wfOf("wtask0001", workflow.StatusKilled, rows...))
	m = nextWF(t, c)
	if m.size > workflowFrameBudget || len(m.Workflow.Agents)+m.RowsOmitted != workflow.MaxAgents || m.Workflow.Status != workflow.StatusKilled {
		t.Fatalf("2000 stopped rows: %d bytes, %d rows, %d omitted", m.size, len(m.Workflow.Agents), m.RowsOmitted)
	}
	failed := 0
	for _, a := range m.Workflow.Agents {
		if a.State == workflow.AgentFailed {
			failed++
		}
	}
	if failed != 3 {
		t.Errorf("%d failed rows in the frame, want all 3 before any stopped one", failed)
	}
}

// The loop answers to its subscription, not the event stream: with the
// CLI's event stream closed and eventPushLoop waiting for a new process,
// the end of the CLI still reaches the client as interrupted; a shim that
// lost its socket shows snapshot_stale until a process reports the run.
func TestWorkflowPush_OutlivesEventResubscribe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		end      cli.ProcessEnd
		status   workflow.Status
		degraded string
	}{
		{"cli exited", cli.ProcessEnd{}, workflow.StatusInterrupted, ""},
		{"shim socket lost", cli.ProcessEnd{ShimLive: true}, workflow.StatusRunning, workflow.DegradedSnapshotStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, router := newWorkflowHub(t)
			proc := session.NewTestProcess()
			sess := router.InjectSession(wfKey, proc)
			src := bindSource(sess)
			src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning)))
			c := newTestWSClient()
			hub.register(c)
			hub.handleSubscribe(c, node.ClientMsg{Type: "subscribe", Key: wfKey})
			nextWF(t, c)
			nextWF(t, c)

			proc.EventLog.CloseSubscribers()
			src.End(tc.end)
			m := nextWF(t, c)
			if m.Workflow.Status != tc.status || m.Workflow.Degraded != tc.degraded {
				t.Fatalf("frame = %+v, want %s/%q", m.Workflow, tc.status, tc.degraded)
			}
			if tc.end.ShimLive {
				bindSource(sess).Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning)))
				if m := nextWF(t, c); m.Workflow.Status != workflow.StatusRunning || m.Workflow.Degraded != "" {
					t.Fatalf("after the reattach: %+v, want running and current", m.Workflow)
				}
			}
		})
	}
}

// Unsubscribing ends the loop within pace.alive though the board is quiet,
// and it sends nothing more.
func TestWorkflowPush_EndsWithSubscription(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	c := newTestWSClient()
	done := startWorkflowLoop(t, hub, c, sess)
	nextWF(t, c)

	hub.handleUnsubscribe(c, node.ClientMsg{Type: "unsubscribe", Key: wfKey})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop outlived its subscription")
	}
	src.Publish(wfOf("wtask0001", workflow.StatusRunning))
	noWF(t, c, 100*time.Millisecond)
}

// A new session under the key (/new) brings a new board: the loop follows
// it with an empty workflow_set, then that board's frames.
func TestWorkflowPush_FollowsNewSession(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	bindSource(sess).Publish(wfOf("wtask0001", workflow.StatusRunning))
	c := newTestWSClient()
	startWorkflowLoop(t, hub, c, sess)
	nextWF(t, c)
	nextWF(t, c)

	fresh := router.InjectSession(wfKey, session.NewTestProcess())
	if m := nextWF(t, c); m.Type != "workflow_set" || len(m.TaskIDs) != 0 {
		t.Fatalf("frame = %+v, want an empty workflow_set", m)
	}
	bindSource(fresh).Publish(wfOf("wtask0002", workflow.StatusRunning))
	set := nextWF(t, c)
	if set.Type != "workflow_set" || !slices.Equal(set.TaskIDs, []string{"wtask0002"}) || set.Epoch != fresh.WorkflowBoard().Published().Epoch {
		t.Fatalf("frame = %+v, want the new board's set", set)
	}
	if m := nextWF(t, c); m.TaskID != "wtask0002" || !m.Full || m.Epoch != set.Epoch {
		t.Fatalf("frame = %+v, want the new board's full frame", m)
	}
}

// completeSubscribe starts the loop for a session with a process; the
// suspended answer (no process) starts none.
func TestCompleteSubscribe_WorkflowLoopOnlyWithProcess(t *testing.T) {
	for _, withProc := range []bool{true, false} {
		t.Run(fmt.Sprint("process=", withProc), func(t *testing.T) {
			hub, router := newWorkflowHub(t)
			var proc *session.TestProcess
			if withProc {
				proc = session.NewTestProcess()
			}
			router.InjectSession(wfKey, proc)
			c := newTestWSClient()
			hub.register(c)
			hub.handleSubscribe(c, node.ClientMsg{Type: "subscribe", Key: wfKey})
			if withProc {
				if m := nextWF(t, c); m.Type != "workflow_set" {
					t.Fatalf("frame = %+v, want workflow_set", m)
				}
				return
			}
			noWF(t, c, 300*time.Millisecond)
		})
	}
}

// Strings reach the frame as the Tracker normalized them: a key in any of
// them, padded to straddle its cap, never shows on the wire.
func TestWorkflowPush_NoSecretOnTheWire(t *testing.T) {
	key := "sk-" + strings.Repeat("Zq9X", 12)
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	for _, pad := range []int{0, 25, 55, 110, 190, 390} {
		v := strings.Repeat("x", pad) + " " + key
		lines := []string{
			fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":"wleak0001","task_type":"local_workflow","workflow_name":%s,"description":%s,"session_id":"s"}`, q(v), q(v)),
			fmt.Sprintf(`{"type":"system","subtype":"task_progress","task_id":"wleak0001","description":%s,"summary":%s,"workflow_progress":[`+
				`{"type":"workflow_phase","index":1,"title":%s},`+
				`{"type":"workflow_agent","index":1,"label":%s,"phaseIndex":1,"agentId":"a1","model":%s,"state":%s,"startedAt":1,"lastToolName":%s,"lastToolSummary":%s,"error":%s},`+
				`{"type":"workflow_agent","index":2,"label":"b","phaseIndex":1,"agentId":"a2","state":"error","startedAt":1,"error":{"detail":%s}}]}`,
				q(v), q(v), q(v), q(v), q(v), q(v), q(v), q(v), q(v), q(v)),
			fmt.Sprintf(`{"type":"system","subtype":"task_updated","task_id":"wleak0001","patch":{"status":%s}}`, q(v)),
			fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":"wleak0001","status":"completed","summary":%s}`, q(v)),
		}
		hub, router := newWorkflowHub(t)
		sess := router.InjectSession(wfKey, session.NewTestProcess())
		src := bindSource(sess)
		tr := workflow.New(nil)
		observe(t, tr, lines[0])
		src.Publish(tr.Load().Workflows...)
		c := newTestWSClient()
		startWorkflowLoop(t, hub, c, sess)
		frames := []wfMsg{nextWF(t, c), nextWF(t, c)}
		observe(t, tr, lines[1:]...)
		src.Publish(tr.Load().Workflows...)
		frames = append(frames, nextWF(t, c))
		last := frames[len(frames)-1].Workflow
		if len(last.Agents) != 2 || last.Agents[0].RawState == "" || last.NotifySummary == "" || last.Phases[0].Title == "" {
			t.Fatalf("pad %d: the frames did not carry every field: %+v", pad, last)
		}
		for _, f := range frames {
			b, _ := json.Marshal(f.Workflow)
			if strings.Contains(string(b), "sk-") {
				t.Errorf("pad %d: key stub on the wire: %s", pad, b)
			}
		}
	}
}

func observe(t *testing.T, tr *workflow.Tracker, lines ...string) {
	t.Helper()
	var proto cli.ClaudeProtocol
	for _, line := range lines {
		events, _, err := proto.ReadEvent(line)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for i := range events {
			tr.Observe(&events[i], time.Now())
		}
	}
}

// Publications racing the loop and a resubscribe taking the key over: the
// old loop ends, the new one ends on the newest state (run with -race).
func TestWorkflowPush_RacesPublishAndTakeover(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	c := newTestWSClient()
	done := startWorkflowLoop(t, hub, c, sess)
	stop := make(chan struct{})
	published := make(chan struct{})
	var publishes atomic.Int64
	go func() {
		defer close(published)
		for i := 1; ; i++ {
			w := wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning))
			w.Tokens = int64(i)
			src.Publish(w)
			publishes.Add(1)
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-c.send:
			case <-stop:
				return
			}
		}
	}()
	testhelper.Eventually(t, func() bool { return publishes.Load() >= 2000 }, 5*time.Second, "the publisher did not run")
	gen := subscribeTest(hub, c, wfKey, func() {})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop outlived the takeover")
	}
	close(stop)
	<-published
	<-drained
	loop2 := make(chan struct{})
	go func() {
		defer close(loop2)
		hub.workflowPushLoop(c, wfKey, gen, sess)
	}()
	defer func() { hub.handleUnsubscribe(c, node.ClientMsg{Type: "unsubscribe", Key: wfKey}); <-loop2 }()
	nextWF(t, c)
	want := sess.WorkflowBoard().Published().Workflows[0].Tokens
	if m := nextWF(t, c); m.Workflow.Tokens != want {
		t.Fatalf("the new loop opened on tokens %d, want the newest %d", m.Workflow.Tokens, want)
	}
}

// Each part of the head goes out at once, not after pace.progress.
func TestWorkflowPush_HeadChangesNotPaced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(w *workflow.Workflow)
	}{
		{"status", func(w *workflow.Workflow) { w.Status = workflow.StatusPaused }},
		{"run id", func(w *workflow.Workflow) { w.RunID = "wf_0a1b2c3d-4e5" }},
		{"degraded", func(w *workflow.Workflow) { w.Degraded = workflow.DegradedSnapshotStale }},
		{"source", func(w *workflow.Workflow) { w.Source = workflow.SourceReplay }},
		{"counts", func(w *workflow.Workflow) { w.Counts.Done++ }},
		{"phases", func(w *workflow.Workflow) { w.Phases = append(w.Phases, workflow.Phase{Index: 2, Title: "Merge"}) }},
		{"agents capped", func(w *workflow.Workflow) { w.AgentsCapped = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, router := newWorkflowHub(t)
			sess := router.InjectSession(wfKey, session.NewTestProcess())
			src := bindSource(sess)
			src.Publish(wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning)))
			c := newTestWSClient()
			p := newPush(t, hub, c, sess)
			t0 := time.Now()
			stepOK(t, p, t0)
			nextWF(t, c)
			nextWF(t, c)

			w := wfOf("wtask0001", workflow.StatusRunning, row(0, workflow.AgentRunning))
			tc.change(w)
			src.Publish(w)
			stepOK(t, p, t0.Add(time.Millisecond))
			if n := len(c.send); n != 1 {
				t.Fatalf("%d frames right after the change, want it at once", n)
			}
		})
	}
}

// The loop's timer brings back what a step held: a progress frame once
// pace.progress is out, a frame the deep queue held after pace.retry,
// though nothing on the board changes and pace.alive is far off.
func TestWorkflowPush_TimerSendsHeldFrames(t *testing.T) {
	hub, router := newWorkflowHub(t)
	hub.workflowPace.alive = time.Hour
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	running := func(id string, done int) *workflow.Workflow {
		w := wfOf(id, workflow.StatusRunning, row(0, workflow.AgentRunning))
		w.Counts.Done = done
		return w
	}
	first := running("wtask0001", 0)
	src.Publish(first, running("wtask0002", 0))
	c := newTestWSClient()
	startWorkflowLoop(t, hub, c, sess)
	for range 3 {
		nextWF(t, c)
	}

	paced := running("wtask0002", 0)
	paced.Tokens = 7
	src.Publish(first, paced)
	if m := nextWF(t, c); m.TaskID != "wtask0002" || m.Workflow.Tokens != 7 {
		t.Fatalf("frame = %+v, want the paced progress", m)
	}

	// An end frame passes the depth gate a running one is held at, so its
	// arrival shows the step that held wtask0002 has run.
	fill(c, workflowQueueDepth+1)
	end := wfOf("wtask0001", workflow.StatusCompleted, row(0, workflow.AgentDone))
	src.Publish(end, running("wtask0002", 1))
	testhelper.Eventually(t, func() bool { return len(c.send) == workflowQueueDepth+2 }, 3*time.Second, "no end frame")
	drain(t, c, workflowQueueDepth+1)
	if m := nextWF(t, c); m.TaskID != "wtask0001" || m.Workflow.Status != workflow.StatusCompleted {
		t.Fatalf("frame = %+v, want the end frame", m)
	}
	if m := nextWF(t, c); m.TaskID != "wtask0002" || m.Workflow.Counts.Done != 1 {
		t.Fatalf("frame = %+v, want the held frame", m)
	}
}

// workflow_set has the half-queue gate: a queue too deep for a running
// workflow's frame still takes it.
func TestWorkflowPush_SetPassesTheRunningGate(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	src.Publish(wfOf("wtask0001", workflow.StatusRunning))
	c := newTestWSClient()
	p := newPush(t, hub, c, sess)
	t0 := time.Now()
	stepOK(t, p, t0)
	nextWF(t, c)
	nextWF(t, c)

	depth := cap(c.send)/2 - 1
	fill(c, depth)
	src.Publish(wfOf("wtask0001", workflow.StatusRunning), wfOf("wtask0002", workflow.StatusRunning))
	if next := stepOK(t, p, t0.Add(time.Second)); next != hub.workflowPace.retry {
		t.Fatalf("next %v, want a retry for the held frame", next)
	}
	if n := len(c.send); n != depth+1 {
		t.Fatalf("%d queued, want the placeholders and the workflow_set alone", n)
	}
	drain(t, c, depth)
	if m := nextWF(t, c); m.Type != "workflow_set" || len(m.TaskIDs) != 2 {
		t.Fatalf("frame = %+v, want the new workflow_set", m)
	}
}

// step checks the subscription before it sends, a workflow_set or a task
// frame: once it is taken over, step sends nothing and reports it gone.
func TestWorkflowPush_StepSendsNothingAfterTakeover(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"task frame", []string{"wtask0001"}},
		{"workflow_set", []string{"wtask0001", "wtask0002"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, router := newWorkflowHub(t)
			sess := router.InjectSession(wfKey, session.NewTestProcess())
			src := bindSource(sess)
			src.Publish(wfOf("wtask0001", workflow.StatusRunning))
			c := newTestWSClient()
			p := newPush(t, hub, c, sess)
			t0 := time.Now()
			stepOK(t, p, t0)
			nextWF(t, c)
			nextWF(t, c)

			subscribeTest(hub, c, wfKey, func() {})
			var wfs []*workflow.Workflow
			for _, id := range tc.ids {
				wfs = append(wfs, wfOf(id, workflow.StatusCompleted))
			}
			src.Publish(wfs...)
			if _, ok := p.step(t0.Add(time.Second)); ok {
				t.Fatal("step went on after the takeover")
			}
			if n := len(c.send); n != 0 {
				t.Fatalf("%d frames sent after the takeover", n)
			}
		})
	}
}

// Following a new board forgets what was sent of the old one: a task id
// both boards hold opens with a full frame of the new epoch, though its
// version there is below the one last sent.
func TestWorkflowPush_FollowReopensReusedTask(t *testing.T) {
	hub, router := newWorkflowHub(t)
	sess := router.InjectSession(wfKey, session.NewTestProcess())
	src := bindSource(sess)
	for i := 1; i <= 3; i++ {
		w := wfOf("wtask0001", workflow.StatusRunning)
		w.Tokens = int64(i)
		src.Publish(w)
	}
	c := newTestWSClient()
	p := newPush(t, hub, c, sess)
	t0 := time.Now()
	stepOK(t, p, t0)
	nextWF(t, c)
	old := nextWF(t, c)

	fresh := router.InjectSession(wfKey, session.NewTestProcess())
	bindSource(fresh).Publish(wfOf("wtask0001", workflow.StatusRunning))
	epoch := fresh.WorkflowBoard().Published().Epoch
	if v := fresh.WorkflowBoard().Published().Workflows[0].Version; v >= old.Version {
		t.Fatalf("the new board's version %d is not below the old %d", v, old.Version)
	}
	p.follow()
	stepOK(t, p, t0.Add(time.Second))
	if m := nextWF(t, c); m.Type != "workflow_set" || m.Epoch != epoch {
		t.Fatalf("frame = %+v, want the new board's set", m)
	}
	if m := nextWF(t, c); m.TaskID != "wtask0001" || !m.Full || m.Epoch != epoch {
		t.Fatalf("frame = %+v, want a full frame of the new board", m)
	}
}
