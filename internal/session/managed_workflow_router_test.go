package session

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// wfProc is a test process whose CLI reports workflows through a real
// Tracker; its end is delivered the way *cli.Process's endHook does.
type wfProc struct {
	*TestProcess
	tr *workflow.Tracker
	cb atomic.Pointer[func()]

	endMu sync.Mutex
	onEnd func(cli.ProcessEnd)
	end   *cli.ProcessEnd
}

func newWFProc() *wfProc {
	p := &wfProc{TestProcess: NewTestProcess()}
	p.tr = workflow.New(func() {
		if fn := p.cb.Load(); fn != nil {
			(*fn)()
		}
	})
	return p
}

func (p *wfProc) Workflows() *workflow.Set { return p.tr.Load() }

func (p *wfProc) SetOnWorkflowChange(fn func()) {
	if fn == nil {
		p.cb.Store(nil)
		return
	}
	p.cb.Store(&fn)
}

func (p *wfProc) KnowWorkflowTasks(ids []string) { p.tr.KnowTasks(ids) }

func (p *wfProc) ApplyWorkflowResult(rf *workflow.ResultFile) bool { return p.tr.ApplyResultFile(rf) }

func (p *wfProc) SetOnEnd(fn func(cli.ProcessEnd)) {
	p.endMu.Lock()
	p.onEnd = fn
	end := p.end
	p.end = nil
	p.endMu.Unlock()
	if end != nil {
		fn(*end)
	}
}

// finish ends the process: its CLI is gone unless end says otherwise.
func (p *wfProc) finish(end cli.ProcessEnd) {
	p.SetAlive(false)
	p.endMu.Lock()
	fn := p.onEnd
	if fn == nil {
		p.end = &end
	}
	p.endMu.Unlock()
	if fn != nil {
		fn(end)
	}
}

// observe feeds frames to the Tracker the way the read loop does.
func (p *wfProc) observe(at time.Time, evs ...clievent.Event) {
	for i := range evs {
		p.tr.Observe(&evs[i], at)
	}
}

// setProcess is a setProc that is also a session process.
type setProcess struct {
	*TestProcess
	*setProc
}

func (p *setProc) process() processIface { return setProcess{NewTestProcess(), p} }

func wfStarted(task, name string) clievent.Event {
	return clievent.Event{Type: "system", SubType: "task_started", TaskID: task, TaskType: clievent.TaskTypeWorkflow, WorkflowName: name, SessionID: wfSID}
}

func wfSnapshot(task string, states ...string) clievent.Event {
	items := []clievent.WorkflowItem{{Type: clievent.WorkflowItemPhase, Index: 1, Title: "Ask"}}
	for i, st := range states {
		items = append(items, clievent.WorkflowItem{
			Type: clievent.WorkflowItemAgent, Index: i + 1, Label: fmt.Sprintf("agent %d", i+1), PhaseIndex: 1,
			State: st, AgentID: fmt.Sprintf("a%016x", i+1), QueuedAt: 1000, StartedAt: 1001,
		})
	}
	return clievent.Event{Type: "system", SubType: "task_progress", TaskID: task, Description: "Ask: agent 1", WorkflowProgress: items, SessionID: wfSID}
}

func wfRouter(t *testing.T) *Router {
	t.Helper()
	r := NewRouter(RouterConfig{Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	t.Cleanup(r.Shutdown)
	return r
}

// within fails the test when fn does not return in time: a deadlock.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return: deadlock", what)
	}
}

// TestWorkflowBoard_BindInsideTransaction: installFreshSession and rename
// bind inside the table transaction, with the Tracker already ahead of the
// board. The bind returns, and exactly one structural sessions_update goes
// out — on the notifier, which takes the table lock itself.
func TestWorkflowBoard_BindInsideTransaction(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	rig := newWFRig(t)
	s.workflows.Store(rig.b)
	proc := newWFProc()
	proc.observe(time.UnixMilli(wfT0), wfStarted("w1", "probe"), wfSnapshot("w1", "start", "queued"))

	var structural atomic.Int32
	within(t, "bind inside r.ss.Update", func() {
		r.ss.Update(func(sessTx) {
			bookWorkflows(s, proc, "", func() { r.ss.Update(markChanged); r.notifyChange(); structural.Add(1) }, r.BumpVersion)
		})
	})
	if d, _ := rig.timer.last(); d != 0 {
		t.Fatalf("bind armed %v, want an immediate structural emission", d)
	}
	within(t, "the structural emission", rig.fire)
	rig.fire()
	if n := structural.Load(); n != 1 {
		t.Errorf("%d structural emissions, want exactly 1", n)
	}
	var dirty bool
	r.ss.View(func(v sessView) { dirty = v.Dirty() })
	if !dirty {
		t.Error("a structural change must mark the store dirty (the Refs persist)")
	}
	if got := s.WorkflowBoard().Published().Workflows; len(got) != 1 || len(got[0].Agents) != 2 {
		t.Fatalf("published %+v, want the frames read before the bind", got)
	}
}

// TestWorkflowBoard_WakeAndBindInterleave: the read loop's wakes (board →
// table via the notifier) and binds inside transactions (table → board)
// interleave without deadlock, and the board ends on the Tracker's latest.
func TestWorkflowBoard_WakeAndBindInterleave(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	a, b := newWFProc(), newWFProc()
	notify := func() { r.ss.Update(markChanged); r.notifyChange() }
	const n = 10_000
	within(t, "10k interleaved wakes and binds", func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			a.observe(time.UnixMilli(wfT0), wfStarted("w1", "probe"))
			for i := range n {
				ev := wfSnapshot("w1", "start")
				ev.WorkflowProgress[1].Tokens = int64(i)
				a.observe(time.UnixMilli(wfT0+int64(i)), ev)
			}
		}()
		go func() {
			defer wg.Done()
			for i := range n {
				p := a
				if i%2 == 1 {
					p = b
				}
				r.ss.Update(func(sessTx) { bookWorkflows(s, p, "", notify, r.BumpVersion) })
			}
		}()
		wg.Wait()
	})
	bookWorkflows(s, a, "", notify, r.BumpVersion)
	set := a.Workflows()
	got := s.WorkflowBoard().Published().Workflows
	if len(got) != 1 || got[0].TrackerVersion != set.Workflows[0].TrackerVersion {
		t.Fatalf("board holds %+v, want the Tracker's latest (version %d)", got, set.Workflows[0].TrackerVersion)
	}
}

// TestWorkflowBoard_ReadersNeverTakeTheBoardLock: evictOldest inside its
// transaction, Cleanup, the snapshot and ReleaseIdleProcess read the board
// with atomic loads only.
func TestWorkflowBoard_ReadersNeverTakeTheBoardLock(t *testing.T) {
	r := wfRouter(t)
	proc := newWFProc()
	s := injectSession(r, "feishu:direct:alice:general", proc)
	s.exempt = true
	bookWorkflows(s, proc, "", nil, nil)
	proc.observe(time.Now(), wfStarted("w1", "probe"), wfSnapshot("w1", "start"))
	b := s.WorkflowBoard()
	b.mu.Lock()
	defer b.mu.Unlock()
	within(t, "readers while b.mu is held", func() {
		r.ss.Update(func(tx sessTx) { r.evictOldest(tx) })
		r.Cleanup()
		_ = s.snapshot(false)
		if s.ReleaseIdleProcess() {
			t.Error("released the CLI of a running workflow")
		}
	})
}

// TestWorkflowBoard_EndInsideTransaction: a process that ended before it was
// bound delivers its end synchronously, inside the spawn's transaction; the
// board unbinds it and interrupts its workflow without deadlock.
func TestWorkflowBoard_EndInsideTransaction(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	proc := newWFProc()
	proc.observe(time.UnixMilli(wfT0), wfStarted("w1", "probe"), wfSnapshot("w1", "start"))
	proc.finish(cli.ProcessEnd{})
	within(t, "bind + bookProcessEnd inside r.ss.Update", func() {
		r.ss.Update(func(sessTx) {
			bookWorkflows(s, proc, "", func() { r.ss.Update(markChanged) }, r.BumpVersion)
			bookProcessEnd(s, proc, "")
		})
	})
	b := s.WorkflowBoard()
	b.mu.Lock()
	bound := b.proc != nil
	b.mu.Unlock()
	if bound {
		t.Error("an ended process stays bound")
	}
	if w := b.Published().Workflows; len(w) != 1 || w[0].Status != workflow.StatusInterrupted {
		t.Errorf("published %+v, want w1 interrupted", w)
	}
}

// TestWorkflowBoard_RespawnCarriesTheBoard is §5.8 "携带": the respawned
// session holds the same board, bound to the new process before anything
// else could bind it, so the new CLI's frames reach it.
func TestWorkflowBoard_RespawnCarriesTheBoard(t *testing.T) {
	r := wfRouter(t)
	var procs []*wfProc
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		p := newWFProc()
		procs = append(procs, p)
		return p, nil
	}
	const key = "dashboard:direct:wf:general"
	s1, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	b := s1.WorkflowBoard()
	epoch := b.Published().Epoch
	procs[0].observe(time.Now(), wfStarted("w1", "probe"), wfSnapshot("w1", "start"))
	procs[0].finish(cli.ProcessEnd{})

	s2, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if s2 == s1 || len(procs) != 2 {
		t.Fatalf("premise: no respawn (sessions %p %p, %d spawns)", s1, s2, len(procs))
	}
	if s2.WorkflowBoard() != b || b.Published().Epoch != epoch {
		t.Fatal("the respawned session has another board")
	}
	b.mu.Lock()
	bound := b.proc == workflowNotifier(procs[1])
	b.mu.Unlock()
	if !bound {
		t.Fatal("the carried board is not bound to the new process")
	}
	procs[1].observe(time.Now(), wfStarted("w2", "second"))
	if got := taskIDs(b.Published().Workflows); len(got) != 2 || got[1] != "w2" {
		t.Errorf("board holds %v, want w1 and the new process's w2", got)
	}
	r.Reset(key)
	s3, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if s3.WorkflowBoard() == b || s3.WorkflowBoard().Published().Epoch == epoch {
		t.Error("a reset session kept the old board: /new must start a new epoch")
	}
}

// TestWorkflowBoard_RenameCarriesTheBoard: rename moves the board by pointer
// and rebinding the same process is a no-op.
func TestWorkflowBoard_RenameCarriesTheBoard(t *testing.T) {
	r := wfRouter(t)
	proc := newWFProc()
	old := injectSession(r, "scratch:direct:x:general", proc)
	bookWorkflows(old, proc, "", nil, nil)
	proc.observe(time.Now(), wfStarted("w1", "probe"))
	b := old.WorkflowBoard()
	v := b.Published().Workflows[0].Version
	if !r.RenameSession("scratch:direct:x:general", "dashboard:direct:x:general") {
		t.Fatal("rename failed")
	}
	fresh := r.SessionFor("dashboard:direct:x:general")
	if fresh.WorkflowBoard() != b || b.Published().Workflows[0].Version != v {
		t.Fatal("rename did not carry the board unchanged")
	}
	proc.observe(time.Now(), wfSnapshot("w1", "start"))
	if w := b.Published().Workflows[0]; len(w.Agents) != 1 {
		t.Error("the renamed session's board stopped receiving the process's frames")
	}
	notifyWired(t, r, fresh)
}

// TestWorkflowBoard_PersistRoundTrip: the Refs go into sessions.json with
// snake_case keys and come back as retained entries; ids that fail their
// check are dropped, empty ones kept.
func TestWorkflowBoard_PersistRoundTrip(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	s.setSessionID(wfSID)
	p := &setProc{}
	bookWorkflows(s, p.process(), "", nil, nil)
	ok := wfEntry("w1", workflow.StatusRunning)
	ok.RunID, ok.SessionID, ok.Name, ok.StartedAt = wfRun, wfSID, "probe", 100
	done := wfEntry("w2", workflow.StatusCompleted)
	done.EndedAt = wfT0
	p.publish(ok, done)
	entry, persisted := sessionToStoreEntry(s)
	if !persisted || len(entry.Workflows) != 2 {
		t.Fatalf("store entry workflows %+v", entry.Workflows)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"workflows":[`, `"task_id":"w1"`, `"run_id":"` + wfRun, `"last_observed_at":`, `"session_id":"` + wfSID} {
		if !strings.Contains(string(data), k) {
			t.Errorf("sessions.json entry lacks %s: %s", k, data)
		}
	}
	tampered := strings.Replace(string(data), `"task_id":"w2"`, `"task_id":"w2","session_id":"../etc"`, 1)
	var back storeEntry
	if err := json.Unmarshal([]byte(tampered), &back); err != nil {
		t.Fatal(err)
	}
	r2 := wfRouter(t)
	r2.ss.Update(func(tx sessTx) { r2.restoreSessionFromEntry(tx, back.Key, &back) })
	got := r2.SessionFor(back.Key).WorkflowBoard().Published().Workflows
	if len(got) != 1 {
		t.Fatalf("restored %v, want w1 only (w2's session id fails its check)", taskIDs(got))
	}
	w := got[0]
	if w.Name != "probe" || w.RunID != wfRun || w.StartedAt != 100 || w.LastObservedAt != wfT0 || w.Source != workflow.SourceRef || w.Degraded != workflow.DegradedSnapshotStale {
		t.Errorf("restored %+v", w)
	}
}

// TestWorkflowBoard_SnapshotCarriesSummaries: /api/sessions shows the
// summaries, snake_case, with epoch and version, with or without a process.
func TestWorkflowBoard_SnapshotCarriesSummaries(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	p := &setProc{}
	bookWorkflows(s, p.process(), "", nil, nil)
	w := wfEntry("w1", workflow.StatusRunning)
	w.Current = "Ask: A"
	w.Counts = workflow.Counts{Total: 3, Running: 1, Queued: 2}
	p.publish(w)
	data, err := json.Marshal(s.snapshot(false))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`"workflows":[{"task_id":"w1","status":"running","counts":{"total":3,"queued":2,"running":1,"done":0,"failed":0,"skipped":0,"stopped":0},"current_phase":"Ask: A","epoch":%q,"version":1}]`, s.WorkflowBoard().Published().Epoch)
	if !strings.Contains(string(data), want) {
		t.Errorf("snapshot JSON lacks\n%s\nin\n%s", want, data)
	}
}

// TestWorkflowBoard_CountUpdateBumpsGen: a count-only sessions_update
// advances the table's version (so a WS-connected dashboard re-renders)
// without dirtying the store; a structural one dirties it.
func TestWorkflowBoard_CountUpdateBumpsGen(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	rig := newWFRig(t)
	s.workflows.Store(rig.b)
	p := &setProc{}
	bookWorkflows(s, p.process(), "", func() { r.ss.Update(markChanged); r.notifyChange() }, r.BumpVersion)
	p.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentRunning)))
	rig.fire()
	r.ss.Update(func(tx sessTx) { tx.SetDirty(false) })
	gen := r.ss.Gen()
	row := wfRow(1, workflow.AgentRunning)
	row.Tokens = 7
	p.publish(wfEntry("w1", workflow.StatusRunning, row))
	rig.advance(workflowSummaryMinInterval)
	rig.fire()
	var dirty bool
	r.ss.View(func(v sessView) { dirty = v.Dirty() })
	if r.ss.Gen() == gen || dirty {
		t.Errorf("after a count update: gen %d→%d, dirty %v; want the gen advanced, the store clean", gen, r.ss.Gen(), dirty)
	}
}

// TestWorkflowBoard_NilBoardSessions: discovery stubs and injected
// sessions have no board; every reader treats them as workflow-free.
func TestWorkflowBoard_NilBoardSessions(t *testing.T) {
	r := wfRouter(t)
	r.RegisterForResume("dashboard:direct:stub:general", wfSID, t.TempDir(), "")
	stub := r.SessionFor("dashboard:direct:stub:general")
	injected := r.InjectSession("feishu:direct:bob:general", nil)
	for _, s := range []*ManagedSession{stub, injected} {
		if s.WorkflowBoard() != nil {
			t.Fatalf("%s has a board", s.key)
		}
		if s.snapshot(false).Workflows != nil || s.workflowPinned(time.Now()) {
			t.Errorf("%s reports workflows", s.key)
		}
	}
	proc := newWFProc()
	bookProcessEnd(injected, proc, "")
	proc.finish(cli.ProcessEnd{}) // the fan-out reaches a nil board
}

// TestWorkflowPinned_KeepAlive: a running workflow keeps an idle session's
// CLI from release, idle expiry and LRU eviction (while another session can
// go), for workflowPinMax after its last frame.
func TestWorkflowPinned_KeepAlive(t *testing.T) {
	r := wfRouter(t)
	r.ttl = time.Minute
	pinnedProc := newWFProc()
	pinned := injectSession(r, "feishu:direct:alice:general", pinnedProc)
	pinned.exempt = true
	pinned.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())
	p := &setProc{}
	bookWorkflows(pinned, p.process(), "", nil, nil)
	running := wfEntry("w1", workflow.StatusRunning)
	running.LastObservedAt = time.Now().Add(-30 * time.Minute).UnixMilli()
	p.publish(running)

	if !pinned.workflowPinned(time.Now()) {
		t.Fatal("a running workflow observed 30m ago does not pin")
	}
	if pinned.ReleaseIdleProcess() {
		t.Fatal("ReleaseIdleProcess closed the CLI of a running workflow")
	}
	pinned.exempt = false
	r.Cleanup()
	if !pinnedProc.Alive() {
		t.Fatal("Cleanup expired the CLI of a running workflow")
	}

	other := injectSession(r, "feishu:direct:bob:general", newIdleProc())
	other.lastActive.Store(time.Now().UnixNano())
	r.ss.Update(func(tx sessTx) {
		if !takeoverHasSlot(tx.View, "x", 2) {
			t.Error("takeoverHasSlot: an idle session must count as evictable")
		}
		r.evictOldest(tx)
	})
	if !pinnedProc.Alive() || other.loadProcess().Alive() {
		t.Fatal("evictOldest took the workflow session over an idle one")
	}
	r.ss.Update(func(tx sessTx) { r.evictOldest(tx) })
	if pinnedProc.Alive() {
		t.Fatal("with only the workflow session idle, evictOldest must fall back to it")
	}

	stale := wfEntry("w1", workflow.StatusRunning)
	stale.LastObservedAt = time.Now().Add(-workflowPinMax - time.Minute).UnixMilli()
	p.publish(stale)
	if pinned.workflowPinned(time.Now()) {
		t.Error("a workflow unobserved for workflowPinMax still pins")
	}
}

// TestWorkflowPinned_CleanupExpiresAfterPinMax: once the workflow has gone
// unobserved for workflowPinMax the idle TTL applies again.
func TestWorkflowPinned_CleanupExpiresAfterPinMax(t *testing.T) {
	r := wfRouter(t)
	r.ttl = time.Minute
	proc := newWFProc()
	s := injectSession(r, "feishu:direct:alice:general", proc)
	s.lastActive.Store(time.Now().Add(-8 * time.Hour).UnixNano())
	p := &setProc{}
	bookWorkflows(s, p.process(), "", nil, nil)
	w := wfEntry("w1", workflow.StatusRunning)
	w.LastObservedAt = time.Now().Add(-7 * time.Hour).UnixMilli()
	p.publish(w)
	r.Cleanup()
	testhelper.Eventually(t, func() bool { return !proc.Alive() }, 5*time.Second, "a workflow unobserved for 7h kept the CLI alive")
}

// TestScratchLastActivity_WorkflowFrames: a scratch ages from its last
// workflow frame, not from its last send; turnOutstanding is unchanged.
func TestScratchLastActivity_WorkflowFrames(t *testing.T) {
	r := wfRouter(t)
	proc := newWFProc()
	s := injectSession(r, "scratch:direct:x:general", proc)
	s.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())
	p := &setProc{}
	bookWorkflows(s, p.process(), "", nil, nil)
	obs := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	w := wfEntry("w1", workflow.StatusRunning)
	w.LastObservedAt = obs.UnixMilli()
	p.publish(w)
	pool := &ScratchPool{router: r}
	if got := pool.lastActivity(s.key, time.Now()); !got.Equal(obs) {
		t.Errorf("lastActivity = %v, want the workflow's last frame %v", got, obs)
	}
	if s.turnOutstanding(proc) {
		t.Error("a running workflow made a turn outstanding")
	}
}

// TestWorkflowBoard_StoreCacheSeesWorkflows: the per-session marshal cache
// re-encodes when only the workflows changed.
func TestWorkflowBoard_StoreCacheSeesWorkflows(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	s.setSessionID(wfSID)
	p := &setProc{}
	bookWorkflows(s, p.process(), "", nil, nil)
	p.publish(wfEntry("w1", workflow.StatusRunning))
	if data, _ := encodeStoreEntryCached(s); !strings.Contains(string(data), `"status":"running"`) {
		t.Fatalf("first encoding %s", data)
	}
	p.publish(wfEntry("w1", workflow.StatusCompleted))
	if data, _ := encodeStoreEntryCached(s); !strings.Contains(string(data), `"status":"completed"`) {
		t.Errorf("cached encoding kept the old workflow: %s", data)
	}
}

// TestWorkflowBoard_RefsDropLongUnclaimed: an unknown entry unobserved for
// workflowPinMax no longer goes into sessions.json; a recent one does.
func TestWorkflowBoard_RefsDropLongUnclaimed(t *testing.T) {
	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	old := wfEntry("u1", workflow.StatusUnknown)
	old.LastObservedAt = wfT0 - workflowPinMax.Milliseconds() - 1
	p.publish(old, wfEntry("u2", workflow.StatusUnknown))
	refs := r.b.refs(r.now())
	if len(refs) != 1 || refs[0].TaskID != "u2" {
		t.Errorf("refs %+v, want u2 only", refs)
	}
}

// notifyWired asserts s's board carries the router's closures: count is
// BumpVersion (the table version moves, the store stays clean), structural
// dirties the store. The count closure is checked by identity: a reattach
// leaves notifications of its own in flight that dirty the store too.
func notifyWired(t *testing.T, r *Router, s *ManagedSession) {
	t.Helper()
	fns := s.WorkflowBoard().notify.fns.Load()
	if fns == nil || fns.count == nil || fns.structural == nil {
		t.Fatal("the board has no sessions_update closures")
	}
	if reflect.ValueOf(fns.count).Pointer() != reflect.ValueOf(r.BumpVersion).Pointer() {
		t.Error("count closure is not Router.BumpVersion")
	}
	r.ss.Update(func(tx sessTx) { tx.SetDirty(false) })
	fns.structural()
	var dirty bool
	r.ss.View(func(v sessView) { dirty = v.Dirty() })
	if !dirty {
		t.Error("structural closure left the store clean")
	}
}

// TestWorkflowBoard_NotifyWiring: spawn and restore wire the router's
// sessions_update closures into the board.
func TestWorkflowBoard_NotifyWiring(t *testing.T) {
	r := wfRouter(t)
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return newWFProc(), nil }
	s, _, err := r.GetOrCreate(context.Background(), "dashboard:direct:wf:general", AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	notifyWired(t, r, s)
	entry := &storeEntry{Key: "feishu:direct:alice:general", SessionID: wfSID}
	r.ss.Update(func(tx sessTx) { r.restoreSessionFromEntry(tx, entry.Key, entry) })
	notifyWired(t, r, r.SessionFor(entry.Key))
}

// TestWorkflowPinned_OnlyRunningObservationsCount: a run that just ended
// does not lend its fresh observation to a stale running one.
func TestWorkflowPinned_OnlyRunningObservationsCount(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	p := &setProc{}
	bookWorkflows(s, p.process(), "", nil, nil)
	stale := wfEntry("w1", workflow.StatusRunning)
	stale.LastObservedAt = time.Now().Add(-workflowPinMax - time.Minute).UnixMilli()
	done := wfEntry("w2", workflow.StatusCompleted)
	done.LastObservedAt, done.EndedAt = time.Now().UnixMilli(), time.Now().UnixMilli()
	p.publish(stale, done)
	if s.workflowPinned(time.Now()) {
		t.Error("a stale running workflow pins because another just ended")
	}
}

// TestWorkflowBoard_ResultMergeRacesObserve: a result-file merge (its
// callback runs on the merging goroutine) racing the read loop's frames
// leaves the board on the Tracker's latest entry.
func TestWorkflowBoard_ResultMergeRacesObserve(t *testing.T) {
	r := wfRouter(t)
	s := injectSession(r, "feishu:direct:alice:general", nil)
	proc := newWFProc()
	bookWorkflows(s, proc, "", nil, nil)
	proc.observe(time.Now(), wfStarted("w1", "probe"))
	within(t, "merges racing frames", func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				ev := wfSnapshot("w1", "start")
				ev.WorkflowProgress[1].Tokens = int64(i)
				proc.observe(time.Now(), ev)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				proc.ApplyWorkflowResult(&workflow.ResultFile{TaskID: "w1", Status: "completed", StartTime: wfT0})
			}
		}()
		wg.Wait()
	})
	want := proc.Workflows().Workflows[0]
	got := s.WorkflowBoard().Published().Workflows[0]
	if got.TrackerVersion != want.TrackerVersion || got.Status != workflow.StatusCompleted {
		t.Errorf("board at tracker version %d (%s), want %d (%s)", got.TrackerVersion, got.Status, want.TrackerVersion, want.Status)
	}
}

// boundProc is the board's bound process, nil when unbound.
func boundProc(b *WorkflowBoard) workflowNotifier {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.proc
}

// TestWorkflowBoard_RenameAfterEndStaysEnded: renaming a session whose CLI
// died, its end already settled, does not bind the dead process again: the
// run stays interrupted and the board unbound.
func TestWorkflowBoard_RenameAfterEndStaysEnded(t *testing.T) {
	r := wfRouter(t)
	proc := newWFProc()
	old := injectSession(r, "scratch:direct:x:general", proc)
	bookWorkflows(old, proc, "", nil, nil)
	bookProcessEnd(old, proc, "")
	proc.observe(time.Now(), wfStarted("w1", "probe"), wfSnapshot("w1", "start"))
	proc.finish(cli.ProcessEnd{})
	b := old.WorkflowBoard()
	if w := b.Published().Workflows[0]; w.Status != workflow.StatusInterrupted {
		t.Fatalf("premise: %s after the CLI exited, want interrupted", w.Status)
	}
	if !r.RenameSession("scratch:direct:x:general", "dashboard:direct:x:general") {
		t.Fatal("rename failed")
	}
	if w := b.Published().Workflows[0]; w.Status != workflow.StatusInterrupted || boundProc(b) != nil || b.Running() {
		t.Errorf("after rename: %s, bound %v, running %v; want interrupted and unbound", w.Status, boundProc(b) != nil, b.Running())
	}
	b.bind(newWFProc(), "")
	b.mu.Lock()
	kept := b.ended != nil
	b.mu.Unlock()
	if kept {
		t.Error("the next process's bind left the board holding the dead one")
	}
}

// TestWorkflowBoard_BindBeforeEndAtEveryInstall: at each site that hands a
// session its process, a process that ended before it was handed over (its
// end not yet delivered) is bound before bookProcessEnd delivers that end,
// so the board unbinds it and interrupts its run.
func TestWorkflowBoard_BindBeforeEndAtEveryInstall(t *testing.T) {
	ended := func() *wfProc {
		p := newWFProc()
		p.observe(time.Now(), wfStarted("w1", "probe"), wfSnapshot("w1", "start"))
		p.finish(cli.ProcessEnd{})
		return p
	}
	check := func(t *testing.T, s *ManagedSession) {
		t.Helper()
		b := s.WorkflowBoard()
		if w := b.Published().Workflows; len(w) != 1 || w[0].Status != workflow.StatusInterrupted || boundProc(b) != nil {
			t.Errorf("published %v (bound %v), want w1 interrupted and the board unbound", w, boundProc(b) != nil)
		}
	}
	t.Run("spawn", func(t *testing.T) {
		r := wfRouter(t)
		r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return ended(), nil }
		s, _, err := r.GetOrCreate(context.Background(), "dashboard:direct:wf:general", AgentOpts{})
		if err != nil {
			t.Fatal(err)
		}
		check(t, s)
	})
	t.Run("rename", func(t *testing.T) {
		r := wfRouter(t)
		injectSession(r, "scratch:direct:x:general", ended())
		if !r.RenameSession("scratch:direct:x:general", "dashboard:direct:x:general") {
			t.Fatal("rename failed")
		}
		check(t, r.SessionFor("dashboard:direct:x:general"))
	})
}

// TestCleanup_WorkflowObservationIsActivity: a finished workflow's recent
// frame counts as the session's activity, so the idle TTL runs from it.
func TestCleanup_WorkflowObservationIsActivity(t *testing.T) {
	r := wfRouter(t)
	r.ttl = time.Minute
	proc := newWFProc()
	s := injectSession(r, "feishu:direct:alice:general", proc)
	s.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())
	p := &setProc{}
	bookWorkflows(s, p.process(), "", nil, nil)
	done := wfEntry("w1", workflow.StatusCompleted)
	done.LastObservedAt, done.EndedAt = time.Now().Add(-10*time.Second).UnixMilli(), time.Now().UnixMilli()
	p.publish(done)
	if s.workflowPinned(time.Now()) {
		t.Fatal("premise: a completed workflow must not pin")
	}
	r.Cleanup()
	if !proc.Alive() {
		t.Error("Cleanup expired a CLI whose workflow reported 10s ago")
	}
}

// TestSnapshot_WorkflowActivity is Q12: between turns the activity line is
// the latest started running workflow's progress; a running turn keeps its
// tool activity, and a session with none running (ended or unknown) keeps
// the last one.
func TestSnapshot_WorkflowActivity(t *testing.T) {
	running := func(id, name string, started int64, done, total int) *workflow.Workflow {
		w := wfEntry(id, workflow.StatusRunning)
		w.Name, w.StartedAt, w.Counts = name, started, workflow.Counts{Total: total, Done: done}
		return w
	}
	paused := running("w5", "held", wfT0, 2, 3)
	paused.Status = workflow.StatusPaused
	ended := wfEntry("w4", workflow.StatusCompleted)
	ended.Name, ended.StartedAt, ended.EndedAt = "finished", wfT0+3, wfT0+4

	cases := []struct {
		name  string
		state cli.ProcessState
		wfs   []*workflow.Workflow
		want  string
	}{
		{"idle parent, latest started wins", cli.StateReady, []*workflow.Workflow{ended, running("w1", "older", wfT0, 1, 1), running("w2", "probe", wfT0+1, 5, 8), running("w6", "unstamped", 0, 0, 1)}, "Workflow probe · 5/8"},
		{"unnamed workflow", cli.StateReady, []*workflow.Workflow{running("w3", "", wfT0, 0, 2)}, "Workflow · 0/2"},
		{"paused counts as running", cli.StateReady, []*workflow.Workflow{paused}, "Workflow held · 2/3"},
		{"running turn keeps its tool", cli.StateRunning, []*workflow.Workflow{running("w2", "probe", wfT0, 5, 8)}, "Read · main.go"},
		{"none running", cli.StateReady, []*workflow.Workflow{ended, wfEntry("w7", workflow.StatusUnknown)}, "Read · main.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tp := NewTestProcess()
			tp.EventLog.Append(clievent.EventEntry{Type: "tool_use", Summary: "Read · main.go"})
			p := &setProc{}
			proc := setProcess{tp, p}
			s := injectSession(wfRouter(t), "feishu:direct:alice:general", proc)
			// Cleanups run last first: the turn ends before Shutdown,
			// which would otherwise wait it out.
			tp.SetState(tc.state)
			t.Cleanup(func() { tp.SetState(cli.StateReady) })
			bookWorkflows(s, proc, "", nil, nil)
			p.publish(tc.wfs...)
			if got := s.Snapshot().LastActivity; got != tc.want {
				t.Errorf("LastActivity = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkflowSnapshotRefresh: a count change reaches /api/sessions as a
// version bump at most workflowSummaryMinInterval later, carrying the new
// counts and activity line; a terminal status bumps it at once.
func TestWorkflowSnapshotRefresh(t *testing.T) {
	r := wfRouter(t)
	p := &setProc{}
	proc := setProcess{NewTestProcess(), p}
	const key = "feishu:direct:alice:general"
	s := injectSession(r, key, proc)
	rig := newWFRig(t)
	s.workflows.Store(rig.b)
	bookWorkflows(s, proc, "", func() { r.ss.Update(markChanged); r.notifyChange() }, r.BumpVersion)
	wf := func(st workflow.Status, done int) *workflow.Workflow {
		w := wfEntry("w1", st)
		w.Name, w.Counts = "probe", workflow.Counts{Total: 3, Done: done}
		return w
	}
	listed := func() (SessionSnapshot, uint64) {
		t.Helper()
		snaps, v := r.ListSessionsWithVersion()
		for _, sn := range snaps {
			if sn.Key == key {
				return sn, v
			}
		}
		t.Fatalf("%s not listed", key)
		return SessionSnapshot{}, 0
	}
	p.publish(wf(workflow.StatusRunning, 1))
	rig.fire()
	_, v0 := listed()

	p.publish(wf(workflow.StatusRunning, 2))
	if d, _ := rig.timer.last(); d <= 0 || d > workflowSummaryMinInterval {
		t.Fatalf("count change armed %v, want within %v", d, workflowSummaryMinInterval)
	}
	if _, v := listed(); v != v0 {
		t.Errorf("version moved %d -> %d before the count emission", v0, v)
	}
	rig.advance(workflowSummaryMinInterval)
	rig.fire()
	snap, v1 := listed()
	if v1 <= v0 || len(snap.Workflows) != 1 || snap.Workflows[0].Counts.Done != 2 || snap.LastActivity != "Workflow probe · 2/3" {
		t.Fatalf("after the count emission: version %d -> %d, workflows %+v, activity %q", v0, v1, snap.Workflows, snap.LastActivity)
	}

	p.publish(wf(workflow.StatusCompleted, 3))
	if d, _ := rig.timer.last(); d != 0 {
		t.Fatalf("terminal status armed %v, want an immediate emission", d)
	}
	rig.fire()
	snap, v2 := listed()
	if v2 <= v1 || snap.Workflows[0].Status != workflow.StatusCompleted || snap.LastActivity != "" {
		t.Errorf("after the terminal emission: version %d -> %d, status %s, activity %q", v1, v2, snap.Workflows[0].Status, snap.LastActivity)
	}
}
