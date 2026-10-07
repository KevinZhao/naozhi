package session

import (
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const (
	wfT0   = int64(1_791_000_000_000)
	wfSID  = "04a8fc10-6fa5-4b8e-82ba-621974425917"
	wfSID2 = "11111111-1111-4111-8111-111111111111"
	wfRun  = "wf_2997921d-435"
)

// setProc is a workflowNotifier whose Set the test writes directly. A
// result file merges into its Set as a Tracker's does; onApply runs first.
type setProc struct {
	mu      sync.Mutex
	set     *workflow.Set
	cb      func()
	known   []string
	onApply func(*workflow.ResultFile)
}

func (p *setProc) Workflows() *workflow.Set {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.set == nil {
		return &workflow.Set{}
	}
	return p.set
}

func (p *setProc) SetOnWorkflowChange(fn func()) {
	p.mu.Lock()
	p.cb = fn
	p.mu.Unlock()
}

func (p *setProc) KnowWorkflowTasks(ids []string) {
	p.mu.Lock()
	p.known = append(p.known, ids...)
	p.mu.Unlock()
}

func (p *setProc) ApplyWorkflowResult(rf *workflow.ResultFile) bool {
	p.mu.Lock()
	var wfs []*workflow.Workflow
	merged := false
	if p.set != nil {
		wfs = slices.Clone(p.set.Workflows)
		for i, w := range wfs {
			if n, ok := workflow.MergeResultFile(w, rf); ok {
				wfs[i], merged = n, true
			}
		}
	}
	hook := p.onApply
	p.mu.Unlock()
	if !merged {
		return false
	}
	if hook != nil {
		hook(rf)
	}
	p.publish(wfs...)
	return true
}

// publish makes wfs the next Set and wakes the board, as a Tracker does.
func (p *setProc) publish(wfs ...*workflow.Workflow) {
	p.mu.Lock()
	v := uint64(1)
	if p.set != nil {
		v = p.set.Version + 1
	}
	p.set = &workflow.Set{Workflows: wfs, Version: v}
	cb := p.cb
	p.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func wfEntry(id string, st workflow.Status, rows ...workflow.Agent) *workflow.Workflow {
	return &workflow.Workflow{TaskID: id, Status: st, Agents: rows, LastObservedAt: wfT0, Source: workflow.SourceStream}
}

func wfRow(i int, st workflow.AgentState) workflow.Agent {
	return workflow.Agent{Index: i, Label: fmt.Sprintf("agent %d", i), State: st}
}

// fakeTimer records the notifier's Resets; the test fires it by hand.
type fakeTimer struct {
	mu     sync.Mutex
	resets []time.Duration
}

func (f *fakeTimer) Reset(d time.Duration) bool {
	f.mu.Lock()
	f.resets = append(f.resets, d)
	f.mu.Unlock()
	return true
}

func (f *fakeTimer) last() (time.Duration, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resets) == 0 {
		return -1, 0
	}
	return f.resets[len(f.resets)-1], len(f.resets)
}

// wfRig is a board on a fake clock and timer, counting its notifications.
type wfRig struct {
	b                  *WorkflowBoard
	clock              atomic.Int64
	timer              *fakeTimer
	structural, counts atomic.Int32
}

func newWFRig(t *testing.T) *wfRig {
	t.Helper()
	r := &wfRig{timer: &fakeTimer{}}
	r.clock.Store(wfT0)
	b := newWorkflowBoard("/projects")
	b.now = r.now
	b.notify.now = r.now
	b.notify.newTimer = func(func()) workflowTimer { return r.timer }
	b.io.pool = newIOPool(workflowIOSlots)
	b.disk = workflowDisk{}
	b.setNotify(func() { r.structural.Add(1) }, func() { r.counts.Add(1) })
	r.b = b
	return r
}

func (r *wfRig) now() time.Time            { return time.UnixMilli(r.clock.Load()) }
func (r *wfRig) advance(d time.Duration)   { r.clock.Add(d.Milliseconds()) }
func (r *wfRig) fire()                     { r.b.notify.fire() }
func (r *wfRig) notified() (s, c int32)    { return r.structural.Load(), r.counts.Load() }
func (r *wfRig) pub() []*workflow.Workflow { return r.b.Published().Workflows }

func (r *wfRig) entry(t *testing.T, id string) *workflow.Workflow {
	t.Helper()
	for _, w := range r.pub() {
		if w.TaskID == id {
			return w
		}
	}
	t.Fatalf("task %s not published; have %v", id, taskIDs(r.pub()))
	return nil
}

func taskIDs(wfs []*workflow.Workflow) []string {
	ids := make([]string, len(wfs))
	for i, w := range wfs {
		ids[i] = w.TaskID
	}
	slices.Sort(ids)
	return ids
}

// TestWorkflowBoard_MergePerField is §5.8 step 3: a restored Ref meets a
// replay seed that never saw task_started (ring wrapped). The seed's state,
// rows and counts win; the Ref's name, start, session and run id stand
// against the seed's fallbacks; the seed counts as observed at bind.
func TestWorkflowBoard_MergePerField(t *testing.T) {
	r := newWFRig(t)
	r.b.restore("k", []workflow.Ref{{
		TaskID: "w113pvmto", RunID: wfRun, Name: "probe", SessionID: wfSID, Status: workflow.StatusRunning,
		StartedAt: 100, LastObservedAt: wfT0 - 60_000, Counts: workflow.Counts{Total: 3},
	}}, "/ws", r.now())
	seed := wfEntry("w113pvmto", workflow.StatusRunning, wfRow(1, workflow.AgentDone), wfRow(2, workflow.AgentRunning))
	seed.Name, seed.StartedAt, seed.SessionID, seed.LastObservedAt = "tiny probe", 500, wfSID2, 0
	seed.Src = workflow.FieldSrc{Name: workflow.NameFromSummary, StartedAt: workflow.StartedFromSnapshot, SessionID: workflow.SessionFromProgress}
	seed.Counts = workflow.Counts{Total: 2, Done: 1, Running: 1}
	p := &setProc{}
	p.publish(seed)
	r.advance(time.Minute)
	r.b.bind(p, "/ws")

	w := r.entry(t, "w113pvmto")
	if w.Name != "probe" || w.StartedAt != 100 || w.SessionID != wfSID || w.RunID != wfRun {
		t.Errorf("name/start/session/run = %q/%d/%q/%q, want the Ref's probe/100/%s/%s", w.Name, w.StartedAt, w.SessionID, w.RunID, wfSID, wfRun)
	}
	if w.Status != workflow.StatusRunning || len(w.Agents) != 2 || w.Counts.Total != 2 || w.Degraded != "" {
		t.Errorf("status/rows/counts/degraded = %s/%d/%d/%q, want the seed's running/2/2/\"\"", w.Status, len(w.Agents), w.Counts.Total, w.Degraded)
	}
	if want := r.now().UnixMilli(); w.LastObservedAt != want {
		t.Errorf("LastObservedAt = %d, want max(Ref, bindAt) = %d", w.LastObservedAt, want)
	}
	if !r.b.Running() || r.b.LastObservedAt() != r.now().UnixMilli() {
		t.Errorf("Running/LastObservedAt = %v/%d: a seeded running workflow must pin its CLI", r.b.Running(), r.b.LastObservedAt())
	}
	if !slices.Equal(p.known, []string{"w113pvmto"}) {
		t.Errorf("bind handed the Tracker %v, want the Ref's task id", p.known)
	}

	better := *seed
	better.Name, better.SessionID, better.LastObservedAt = "real", wfSID2, r.now().UnixMilli()
	better.Src.Name, better.Src.SessionID = workflow.NameFromLaunch, workflow.SessionFromLaunch
	p.publish(&better)
	w = r.entry(t, "w113pvmto")
	if w.Name != "real" || w.SessionID != wfSID2 || w.StartedAt != 100 {
		t.Errorf("name/session/start = %q/%q/%d: launch grades beat the Ref, the Ref's start still beats a snapshot's", w.Name, w.SessionID, w.StartedAt)
	}
}

// TestWorkflowBoard_MergeKeepsLaunchDir: a reattached CLI's seed lacks the
// launch receipt the former process read; the retained entry's transcript
// dir fills it in, on the entry and in the run dir's resolve source.
func TestWorkflowBoard_MergeKeepsLaunchDir(t *testing.T) {
	r := newWFRig(t)
	const dir = "/projects/-ws/" + wfSID + "/subagents/workflows/" + wfRun
	a := &setProc{}
	launched := wfEntry("w1", workflow.StatusRunning)
	launched.RunID, launched.LaunchTranscriptDir = wfRun, dir
	a.publish(launched)
	r.b.bind(a, "/ws")
	r.b.procEnded(a, cli.ProcessEnd{ShimLive: true})
	b := &setProc{}
	seed := wfEntry("w1", workflow.StatusRunning)
	seed.RunID = wfRun
	b.publish(seed)
	r.b.bind(b, "/ws")
	r.b.mu.Lock()
	src := r.b.resolve["w1"].src
	r.b.mu.Unlock()
	if w := r.entry(t, "w1"); w.LaunchTranscriptDir != dir || src.TranscriptDir != dir {
		t.Errorf("launch dir %q, resolve source %q; want the retained %s", w.LaunchTranscriptDir, src.TranscriptDir, dir)
	}
}

// TestWorkflowBoard_VersionsAcrossTrackers: a replacing Tracker starts its
// private versions over; wire versions still rise, an unchanged row keeps
// its Rev, a changed one moves to the new version.
func TestWorkflowBoard_VersionsAcrossTrackers(t *testing.T) {
	r := newWFRig(t)
	r.b.restore("k", []workflow.Ref{{TaskID: "w1", Status: workflow.StatusRunning, LastObservedAt: wfT0}}, "/ws", r.now())
	vRef := r.entry(t, "w1").Version

	a := &setProc{}
	for range 5 {
		a.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentDone), wfRow(2, workflow.AgentRunning)))
	}
	r.b.bind(a, "/ws")
	w := r.entry(t, "w1")
	if w.Version <= vRef || w.Agents[0].Rev != w.Version {
		t.Fatalf("version %d after the Ref's %d, row Rev %d: the Tracker entry must publish above the Ref", w.Version, vRef, w.Agents[0].Rev)
	}
	v1, rev1 := w.Version, w.Agents[0].Rev

	r.b.procEnded(a, cli.ProcessEnd{ShimLive: true})
	if got := r.entry(t, "w1"); got.Version <= v1 || got.Degraded != workflow.DegradedSnapshotStale {
		t.Fatalf("after ShimLive: version %d degraded %q, want > %d and snapshot_stale", got.Version, got.Degraded, v1)
	}
	v2 := r.entry(t, "w1").Version

	b := &setProc{} // Set.Version 1 < a's 5
	b.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentDone), wfRow(2, workflow.AgentDone)))
	r.b.bind(b, "/ws")
	w = r.entry(t, "w1")
	if w.Version <= v2 {
		t.Fatalf("version %d after the stale %d: a lower Tracker version must not lower the wire version", w.Version, v2)
	}
	if w.Agents[0].Rev != rev1 || w.Agents[1].Rev != w.Version {
		t.Errorf("row Revs = %d, %d, want %d (unchanged) and %d (changed)", w.Agents[0].Rev, w.Agents[1].Rev, rev1, w.Version)
	}
	if w.Degraded != "" {
		t.Errorf("Degraded = %q, want the live Tracker's", w.Degraded)
	}
}

// TestWorkflowBoard_SharedRowsNotCompared: a header-only frame shares the
// previous rows (copy-on-write); the board keeps their Revs without
// comparing a single row.
func TestWorkflowBoard_SharedRowsNotCompared(t *testing.T) {
	var compared atomic.Int32
	agentEqual = func(a, b *workflow.Agent) bool {
		compared.Add(1)
		return workflow.AgentEqualIgnoringRev(a, b)
	}
	t.Cleanup(func() { agentEqual = workflow.AgentEqualIgnoringRev })

	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	first := wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentDone), wfRow(2, workflow.AgentRunning))
	p.publish(first)
	compared.Store(0)
	header := *first
	header.Current = "Sum: C"
	p.publish(&header)
	if n := compared.Load(); n != 0 {
		t.Errorf("%d row comparisons for a frame sharing the rows, want 0", n)
	}
	changed := *first
	changed.Agents = slices.Clone(first.Agents)
	changed.Agents[1].Tokens = 9
	p.publish(&changed)
	if n := compared.Load(); n != 2 {
		t.Errorf("%d row comparisons for new rows, want 2 (one per index)", n)
	}
	w := r.entry(t, "w1")
	if w.Agents[0].Rev == w.Version || w.Agents[1].Rev != w.Version {
		t.Errorf("Revs = %d, %d at version %d: only the changed row moves", w.Agents[0].Rev, w.Agents[1].Rev, w.Version)
	}
}

// TestWorkflowBoard_PrevAgentIDsMoveRev: a retry that only changes the
// attempt history is a row change.
func TestWorkflowBoard_PrevAgentIDsMoveRev(t *testing.T) {
	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	a := wfRow(1, workflow.AgentRunning)
	a.AgentID = "a2"
	p.publish(wfEntry("w1", workflow.StatusRunning, a))
	before := r.entry(t, "w1").Agents[0].Rev
	a.PrevAgentIDs = []string{"a1"}
	p.publish(wfEntry("w1", workflow.StatusRunning, a))
	if w := r.entry(t, "w1"); w.Agents[0].Rev == before || w.Agents[0].Rev != w.Version {
		t.Errorf("Rev %d → %d at version %d: a PrevAgentIDs change must move the row", before, w.Agents[0].Rev, w.Version)
	}
}

// TestWorkflowBoard_RowUnion: within an epoch an index never disappears.
// A Tracker that lost its snapshot (reconnect, no_snapshot) leaves every row
// published; a row only the board still holds stops when the run ends; the
// next snapshot replaces rows by index.
func TestWorkflowBoard_RowUnion(t *testing.T) {
	r := newWFRig(t)
	a := &setProc{}
	r.b.bind(a, "/ws")
	a.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentDone), wfRow(2, workflow.AgentRunning), wfRow(3, workflow.AgentQueued)))
	revs := func() []uint64 {
		var out []uint64
		for _, row := range r.entry(t, "w1").Agents {
			out = append(out, row.Rev)
		}
		return out
	}
	before := revs()

	b := &setProc{}
	noSnap := wfEntry("w1", workflow.StatusRunning)
	noSnap.Degraded = workflow.DegradedNoSnapshot
	b.publish(noSnap)
	r.b.bind(b, "/ws")
	if w := r.entry(t, "w1"); len(w.Agents) != 3 || !slices.Equal(revs(), before) {
		t.Fatalf("after a snapshot-less Tracker: %d rows, Revs %v (were %v); every row must stay, unchanged", len(w.Agents), revs(), before)
	}

	b.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentDone)))
	if w := r.entry(t, "w1"); len(w.Agents) != 3 || w.Agents[1].State != workflow.AgentRunning {
		t.Fatalf("rows %d, row 2 %s: a snapshot without an index keeps the published row", len(w.Agents), w.Agents[1].State)
	}
	b.publish(wfEntry("w1", workflow.StatusCompleted, wfRow(1, workflow.AgentDone)))
	w := r.entry(t, "w1")
	if w.Agents[1].State != workflow.AgentStopped || w.Agents[2].State != workflow.AgentStopped || w.Agents[1].Rev != w.Version {
		t.Errorf("kept rows %s/%s (Rev %d at version %d): once terminal they stop, at the new version", w.Agents[1].State, w.Agents[2].State, w.Agents[1].Rev, w.Version)
	}
	if w.Agents[0].Rev != before[0] {
		t.Errorf("row 1 Rev %d, want %d: unchanged", w.Agents[0].Rev, before[0])
	}
}

// TestWorkflowBoard_RowUnionCapped: the union holds at most MaxAgents rows.
func TestWorkflowBoard_RowUnionCapped(t *testing.T) {
	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	low := make([]workflow.Agent, workflow.MaxAgents)
	high := make([]workflow.Agent, workflow.MaxAgents)
	for i := range low {
		low[i] = wfRow(i+1, workflow.AgentDone)
		high[i] = wfRow(i+1+workflow.MaxAgents/2, workflow.AgentDone)
	}
	p.publish(wfEntry("w1", workflow.StatusRunning, low...))
	p.publish(wfEntry("w1", workflow.StatusRunning, high...))
	if n := len(r.entry(t, "w1").Agents); n != workflow.MaxAgents {
		t.Errorf("union of two %d-row sets has %d rows, want %d", workflow.MaxAgents, n, workflow.MaxAgents)
	}
}

// TestWorkflowBoard_ProcessEnd is §5.6(6a): a CLI that is gone interrupts
// its running workflows; a lost socket or a detach leaves them running,
// stale and unobserved, until the reattached CLI reports them again.
func TestWorkflowBoard_ProcessEnd(t *testing.T) {
	cases := []struct {
		name string
		end  cli.ProcessEnd
		want workflow.Status
	}{
		{"cli exited", cli.ProcessEnd{}, workflow.StatusInterrupted},
		{"shim live", cli.ProcessEnd{ShimLive: true}, workflow.StatusRunning},
		{"detached", cli.ProcessEnd{Detached: true}, workflow.StatusRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newWFRig(t)
			p := &setProc{}
			r.b.bind(p, "/ws")
			p.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentDone), wfRow(2, workflow.AgentRunning)))
			r.fire()
			s0, _ := r.notified()
			r.advance(time.Minute)
			r.b.procEnded(p, tc.end)
			w := r.entry(t, "w1")
			if w.Status != tc.want {
				t.Fatalf("status = %s, want %s", w.Status, tc.want)
			}
			if r.b.Running() == (tc.want == workflow.StatusInterrupted) {
				t.Errorf("Running = %v after %s", r.b.Running(), tc.name)
			}
			if w.LastObservedAt != wfT0 {
				t.Errorf("LastObservedAt = %d, want it frozen at %d", w.LastObservedAt, wfT0)
			}
			r.fire()
			s1, _ := r.notified()
			if tc.want == workflow.StatusInterrupted {
				if w.Agents[1].State != workflow.AgentStopped || w.EndedAt != r.now().UnixMilli() || s1 != s0+1 {
					t.Errorf("row 2 %s, EndedAt %d, structural %d→%d: interrupted stops live rows, ends now, is structural", w.Agents[1].State, w.EndedAt, s0, s1)
				}
				return
			}
			if w.Degraded != workflow.DegradedSnapshotStale || s1 != s0 {
				t.Errorf("degraded %q, structural %d→%d: want snapshot_stale and no structural change", w.Degraded, s0, s1)
			}
			again := &setProc{}
			again.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentDone), wfRow(2, workflow.AgentRunning)))
			r.b.bind(again, "/ws")
			if w := r.entry(t, "w1"); w.Status != workflow.StatusRunning || w.Degraded != "" {
				t.Errorf("reattached: %s/%q, want live again", w.Status, w.Degraded)
			}
		})
	}
}

// TestWorkflowBoard_EndOfFormerProcess: a process replaced while bound
// leaves its entries stale; its end, arriving after, settles them by cause.
func TestWorkflowBoard_EndOfFormerProcess(t *testing.T) {
	r := newWFRig(t)
	a, b := &setProc{}, &setProc{}
	r.b.bind(a, "/ws")
	a.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentRunning)))
	r.b.bind(b, "/ws")
	if w := r.entry(t, "w1"); w.Status != workflow.StatusRunning || w.Degraded != workflow.DegradedSnapshotStale {
		t.Fatalf("after rebinding: %s/%q, want running and snapshot_stale (a's CLI may live)", w.Status, w.Degraded)
	}
	a.publish(wfEntry("w1", workflow.StatusCompleted))
	if w := r.entry(t, "w1"); w.Status != workflow.StatusRunning {
		t.Fatalf("an unbound process's wake changed the board: %s", w.Status)
	}
	r.b.procEnded(a, cli.ProcessEnd{})
	if w := r.entry(t, "w1"); w.Status != workflow.StatusInterrupted {
		t.Errorf("after a's CLI exited: %s, want interrupted", w.Status)
	}
	b.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentRunning)))
	if w := r.entry(t, "w1"); w.Status != workflow.StatusRunning || w.EndedAt != 0 {
		t.Errorf("b reports the task: %s ended %d, want the live Tracker's running, not ended", w.Status, w.EndedAt)
	}
}

// TestWorkflowBoard_StaleSetIgnored: a Set no newer than the applied one
// never replaces it, and rebinding the bound process changes nothing.
func TestWorkflowBoard_StaleSetIgnored(t *testing.T) {
	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	p.publish(wfEntry("w1", workflow.StatusRunning))
	p.publish(wfEntry("w1", workflow.StatusCompleted))
	newest := p.Workflows()
	p.mu.Lock()
	p.set = &workflow.Set{Workflows: []*workflow.Workflow{wfEntry("w1", workflow.StatusRunning)}, Version: 1}
	p.mu.Unlock()
	r.b.wake(p)
	if w := r.entry(t, "w1"); w.Status != workflow.StatusCompleted {
		t.Fatalf("status %s: an older Set overrode a newer one", w.Status)
	}
	p.mu.Lock()
	p.set = newest
	p.mu.Unlock()
	v := r.entry(t, "w1").Version
	r.b.bind(p, "/elsewhere")
	if w := r.entry(t, "w1"); w.Version != v || r.b.workspace != "/ws" {
		t.Errorf("rebinding the bound process: version %d→%d, workspace %q; want a no-op", v, w.Version, r.b.workspace)
	}
}

// TestWorkflowBoard_TrackerEvictionKept: an entry the process's Set drops
// (its terminal LRU) stays published from retained, unchanged.
func TestWorkflowBoard_TrackerEvictionKept(t *testing.T) {
	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	done := wfEntry("w1", workflow.StatusCompleted, wfRow(1, workflow.AgentDone))
	done.EndedAt = wfT0
	p.publish(done, wfEntry("w2", workflow.StatusRunning))
	v := r.entry(t, "w1").Version
	p.publish(wfEntry("w2", workflow.StatusRunning))
	if w := r.entry(t, "w1"); w.Status != workflow.StatusCompleted || w.Version != v || len(w.Agents) != 1 {
		t.Errorf("evicted entry: %s v%d rows %d, want it kept unchanged (v%d)", w.Status, w.Version, len(w.Agents), v)
	}
}

// TestWorkflowBoard_Capacity: over many binds each leaving terminal and
// running entries behind, the board holds at most 16 unsettled and 5
// terminal ones, and what it forgets it forgets everywhere.
func TestWorkflowBoard_Capacity(t *testing.T) {
	r := newWFRig(t)
	for cycle := range 8 {
		p := &setProc{}
		var wfs []*workflow.Workflow
		for i := range 3 {
			w := wfEntry(fmt.Sprintf("r%d%d", cycle, i), workflow.StatusRunning, wfRow(1, workflow.AgentRunning))
			w.RunID = fmt.Sprintf("wf_%08d-%03d", cycle, i)
			wfs = append(wfs, w)
		}
		for i := range 2 {
			w := wfEntry(fmt.Sprintf("t%d%d", cycle, i), workflow.StatusCompleted, wfRow(1, workflow.AgentDone))
			w.EndedAt = wfT0 + int64(cycle*10+i)
			wfs = append(wfs, w)
		}
		p.publish(wfs...)
		r.advance(time.Second)
		r.b.bind(p, "/ws")
	}
	var unsettled, terminal int
	for _, w := range r.pub() {
		if workflow.IsUnsettled(w.Status) {
			unsettled++
		} else {
			terminal++
		}
	}
	if unsettled != workflowBoardMaxUnsettled || terminal != workflowBoardMaxTerminal {
		t.Fatalf("board holds %d unsettled, %d terminal; want %d and %d", unsettled, terminal, workflowBoardMaxUnsettled, workflowBoardMaxTerminal)
	}
	r.b.mu.Lock()
	nLast, nRet, nRes := len(r.b.last), len(r.b.retained), len(r.b.resolve)
	r.b.mu.Unlock()
	if n := len(r.pub()); nLast != n || nRes != n || nRet > n {
		t.Errorf("last %d, resolve %d, retained %d for %d published: dropped entries must go everywhere", nLast, nRes, nRet, n)
	}
	for _, id := range []string{"r70", "r71", "r72", "t70", "t71"} {
		r.entry(t, id) // the live process's entries are never dropped
	}
	if refs := r.b.refs(r.now()); len(refs) != workflowBoardMaxUnsettled+workflowBoardMaxTerminal {
		t.Errorf("%d refs, want the bounded set's %d", len(refs), workflowBoardMaxUnsettled+workflowBoardMaxTerminal)
	}
}

// TestWorkflowBoard_CapacityDropsUnknownFirst: over the cap, retained
// unknown entries go before stale running ones, however recently observed.
func TestWorkflowBoard_CapacityDropsUnknownFirst(t *testing.T) {
	r := newWFRig(t)
	var refs []workflow.Ref
	for i := range 10 {
		refs = append(refs,
			workflow.Ref{TaskID: fmt.Sprintf("u%d", i), Status: workflow.StatusUnknown, LastObservedAt: wfT0},
			workflow.Ref{TaskID: fmt.Sprintf("s%d", i), Status: workflow.StatusRunning, LastObservedAt: wfT0 - time.Hour.Milliseconds()})
	}
	r.b.restore("k", refs, "/ws", r.now())
	var unknown, stale int
	for _, w := range r.pub() {
		switch w.Status {
		case workflow.StatusUnknown:
			unknown++
		case workflow.StatusRunning:
			stale++
		}
	}
	if unknown != 6 || stale != 10 {
		t.Errorf("kept %d unknown, %d stale running; want 6 and 10: unknown entries are dropped first", unknown, stale)
	}
}

// TestWorkflowBoard_LiveOverCap: a live process holding more unsettled runs
// than the board's cap (16 with rows, 4 header-only) keeps all of them
// running across wakes, without structural churn; only retained entries
// give way; the Refs keep the 16 latest observed.
func TestWorkflowBoard_LiveOverCap(t *testing.T) {
	r := newWFRig(t)
	r.b.restore("k", []workflow.Ref{
		{TaskID: "old1", Status: workflow.StatusRunning, LastObservedAt: wfT0},
		{TaskID: "old2", Status: workflow.StatusRunning, LastObservedAt: wfT0},
		{TaskID: "old3", Status: workflow.StatusUnknown, LastObservedAt: wfT0},
	}, "/ws", r.now())
	p := &setProc{}
	live := func(tick int) []*workflow.Workflow {
		var wfs []*workflow.Workflow
		for i := range 20 {
			w := wfEntry(fmt.Sprintf("w%02d", i), workflow.StatusRunning)
			w.LastObservedAt = wfT0 + int64(i)
			if i < 16 {
				w.Agents = []workflow.Agent{wfRow(1, workflow.AgentRunning)}
				w.Agents[0].Tokens = int64(tick)
			} else {
				w.Degraded = workflow.DegradedTooMany
			}
			wfs = append(wfs, w)
		}
		return wfs
	}
	p.publish(live(0)...)
	r.b.bind(p, "/ws")
	r.fire()
	s0, _ := r.notified()
	for tick := 1; tick <= 5; tick++ {
		p.publish(live(tick)...)
		r.fire()
	}
	if s1, _ := r.notified(); s1 != s0 {
		t.Errorf("structural notifications %d → %d over count-only wakes", s0, s1)
	}
	if got := taskIDs(r.pub()); len(got) != 20 || got[0] != "w00" {
		t.Fatalf("published %v, want exactly the 20 live entries", got)
	}
	for _, w := range r.pub() {
		if w.Status != workflow.StatusRunning {
			t.Errorf("%s is %s: a live entry is never capped to a terminal status", w.TaskID, w.Status)
		}
	}
	refs := r.b.refs(r.now())
	var ids []string
	for _, ref := range refs {
		ids = append(ids, ref.TaskID)
	}
	slices.Sort(ids)
	if len(ids) != 16 || ids[0] != "w04" || ids[15] != "w19" {
		t.Errorf("refs %v, want the 16 latest observed (w04..w19)", ids)
	}
}

// TestWorkflowBoard_Summaries: the display set is every unsettled entry,
// unknown included, plus the three latest terminal ones; computed once per
// publication; paused runs, unknown does not.
func TestWorkflowBoard_Summaries(t *testing.T) {
	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	var wfs []*workflow.Workflow
	for i := range 5 {
		w := wfEntry(fmt.Sprintf("t%d", i), workflow.StatusCompleted)
		w.EndedAt = wfT0 + int64(i)
		wfs = append(wfs, w)
	}
	unknown := wfEntry("u1", workflow.StatusUnknown)
	unknown.Current = "Sum: C"
	p.publish(append(wfs, unknown)...)
	sums := r.b.Summaries()
	var ids []string
	for _, s := range sums {
		ids = append(ids, s.TaskID)
		if s.Epoch != r.b.epoch || s.Version == 0 {
			t.Errorf("summary %s: epoch %q version %d", s.TaskID, s.Epoch, s.Version)
		}
	}
	if !slices.Equal(ids, []string{"u1", "t4", "t3", "t2"}) || sums[0].CurrentPhase != "Sum: C" {
		t.Errorf("summaries %v (phase %q), want u1 then the three latest terminal", ids, sums[0].CurrentPhase)
	}
	if &r.b.Summaries()[0] != &sums[0] {
		t.Error("Summaries recomputed per call; want the publication's slice")
	}
	if r.b.Running() {
		t.Error("Running with only unknown and terminal entries")
	}
	p.publish(wfEntry("p1", workflow.StatusPaused))
	if !r.b.Running() {
		t.Error("Running false with a paused entry")
	}
}

// TestBoardNotifier: structural changes go out at once, count-only ones at
// most once per interval, the last one in a window at its end; neither
// takes the board's lock.
func TestBoardNotifier(t *testing.T) {
	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	p.publish(wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentRunning)))
	if d, _ := r.timer.last(); d != 0 {
		t.Fatalf("a new workflow armed %v, want an immediate emission", d)
	}
	r.b.mu.Lock() // the timer goroutine must not need it
	r.fire()
	r.b.mu.Unlock()
	if s, c := r.notified(); s != 1 || c != 0 {
		t.Fatalf("structural/count = %d/%d, want 1/0", s, c)
	}
	tick := func(tokens int64) {
		row := wfRow(1, workflow.AgentRunning)
		row.Tokens = tokens
		p.publish(wfEntry("w1", workflow.StatusRunning, row))
	}
	tick(1)
	if d, _ := r.timer.last(); d != workflowSummaryMinInterval {
		t.Fatalf("count change armed %v, want the rest of the interval (%v)", d, workflowSummaryMinInterval)
	}
	r.advance(10 * time.Second)
	tick(2)
	_, n := r.timer.last()
	tick(3)
	if _, m := r.timer.last(); m != n {
		t.Error("a pending count emission was re-armed")
	}
	r.fire() // early: the window is still open
	if _, c := r.notified(); c != 0 {
		t.Fatalf("count emitted %v into its window", 10*time.Second)
	}
	if d, _ := r.timer.last(); d != 20*time.Second {
		t.Fatalf("early fire re-armed %v, want 20s", d)
	}
	r.advance(20 * time.Second)
	r.fire()
	if s, c := r.notified(); s != 1 || c != 1 {
		t.Fatalf("structural/count = %d/%d, want 1/1 at the window's end", s, c)
	}
	r.fire()
	if _, c := r.notified(); c != 1 {
		t.Error("a fire with nothing pending emitted")
	}
	p.publish(wfEntry("w1", workflow.StatusCompleted))
	r.fire()
	if s, _ := r.notified(); s != 2 {
		t.Errorf("structural = %d after a terminal status, want 2", s)
	}
}

// TestWorkflowBoard_ResolveOffLock is §5.8 "RunDir 解析": the resolver runs
// on the I/O dispatch, never inside bind or wake; a result for a source
// that changed meanwhile is dropped; the resolved run dir and its session
// id do not move the wire version nor start another resolution.
func TestWorkflowBoard_ResolveOffLock(t *testing.T) {
	r := newWFRig(t)
	type call struct {
		src  workflowRunSource
		done chan workflowRun
	}
	calls := make(chan call, 4)
	var n atomic.Int32
	r.b.disk.resolve = func(_ string, src workflowRunSource) (workflowRun, bool) {
		n.Add(1)
		c := call{src: src, done: make(chan workflowRun)}
		calls <- c
		run := <-c.done
		return run, run.RunDir != ""
	}
	p := &setProc{}
	first := wfEntry("w1", workflow.StatusRunning)
	first.RunID, first.SessionID = "wf_aaaaaaaa-111", wfSID
	p.publish(first)
	returned := make(chan struct{})
	go func() {
		r.b.bind(p, "/ws")
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("bind blocked on the resolver")
	}
	c1 := <-calls
	if c1.src.RunID != "wf_aaaaaaaa-111" || c1.src.Workspace != "/ws" || c1.src.SessionID != wfSID {
		t.Fatalf("resolver source %+v", c1.src)
	}
	second := *first
	second.RunID = "wf_bbbbbbbb-222"
	p.publish(&second) // the read loop: must not block either
	r.b.mu.Lock()
	queued := len(r.b.io.queue)
	r.b.mu.Unlock()
	if queued != 1 {
		t.Fatalf("%d jobs queued behind the task's running one, want 1: one job per task in flight", queued)
	}
	c1.done <- workflowRun{RunDir: "/projects/x/a"}
	c2 := <-calls // starts once c1's result applied: one job per task
	if w := r.entry(t, "w1"); w.RunDir != "" {
		t.Fatalf("RunDir %q from a source that changed during the resolution", w.RunDir)
	}
	c2.done <- workflowRun{RunDir: "/projects/x/b", SessionID: wfSID2}
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").RunDir != "" }, 5*time.Second, "run dir never resolved")
	w := r.entry(t, "w1")
	if w.RunDir != "/projects/x/b" || w.SessionID != wfSID2 || w.Src.SessionID != workflow.SessionFromRunDir {
		t.Fatalf("RunDir %q session %q (grade %d), want the current source's result", w.RunDir, w.SessionID, w.Src.SessionID)
	}
	v := w.Version
	third := second
	third.Current = "Sum: C"
	p.publish(&third)
	if got := r.entry(t, "w1"); got.Version == v || got.RunDir != "/projects/x/b" {
		t.Errorf("later change: version %d (was %d), RunDir %q", got.Version, v, got.RunDir)
	}
	// Folded into retained (the socket broke), the entry keeps its source.
	r.b.procEnded(p, cli.ProcessEnd{ShimLive: true})
	r.b.mu.Lock()
	pending := len(r.b.io.inflight) + len(r.b.io.queue)
	r.b.mu.Unlock()
	if n.Load() != 2 || pending != 0 {
		t.Errorf("%d resolver calls, %d pending; want 2 and 0: the run dir's session id must not feed back into the source", n.Load(), pending)
	}
	if refs := r.b.refs(r.now()); refs[0].SessionID != wfSID2 {
		t.Errorf("Ref session id %q, want the run dir's %s", refs[0].SessionID, wfSID2)
	}
}

// TestWorkflowBoard_IOBounds: two jobs in flight per board, eight across
// boards; a board whose resolver hangs does not hold up another's.
func TestWorkflowBoard_IOBounds(t *testing.T) {
	pool := newIOPool(workflowIOSlots)
	release := make(chan struct{})
	var inflight, peak, calls atomic.Int32
	var peakMu sync.Mutex
	hang := func(string, workflowRunSource) (workflowRun, bool) {
		calls.Add(1)
		cur := inflight.Add(1)
		peakMu.Lock()
		peak.Store(max(peak.Load(), cur))
		peakMu.Unlock()
		<-release
		inflight.Add(-1)
		return workflowRun{}, false
	}
	var boards []*WorkflowBoard
	for i := range 6 {
		b := newWorkflowBoard("/projects")
		b.io.pool, b.disk = pool, workflowDisk{resolve: hang}
		p := &setProc{}
		var wfs []*workflow.Workflow
		for j := range 3 {
			w := wfEntry(fmt.Sprintf("w%d", j), workflow.StatusRunning)
			w.RunID = fmt.Sprintf("wf_%08d-%03d", i, j)
			wfs = append(wfs, w)
		}
		p.publish(wfs...)
		b.bind(p, "/ws")
		boards = append(boards, b)
	}
	testhelper.Eventually(t, func() bool { return inflight.Load() == workflowIOSlots }, 5*time.Second, "global slots never filled")
	if got := peak.Load(); got != workflowIOSlots {
		t.Errorf("peak in flight %d, want the global %d", got, workflowIOSlots)
	}
	for _, b := range boards {
		b.mu.Lock()
		if n := len(b.io.inflight); n > workflowBoardIOInFlight {
			t.Errorf("a board has %d jobs in flight, want ≤ %d", n, workflowBoardIOInFlight)
		}
		b.mu.Unlock()
	}
	close(release)
	// Boards that found no free slot are pumped as slots free up.
	testhelper.Eventually(t, func() bool { return calls.Load() == 6*3 }, 5*time.Second, "a board waiting for a global slot was never pumped")
}

// acquireBlockedOnPoolMu reports whether a goroutine sits in pool's acquire
// waiting for pool.mu.
func acquireBlockedOnPoolMu(pool *ioPool) bool {
	buf := make([]byte, 8<<20)
	buf = buf[:runtime.Stack(buf, true)]
	frame := fmt.Sprintf("(*ioPool).acquire(%p", pool)
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, frame) && strings.Contains(g, "Mutex).Lock") {
			return true
		}
	}
	return false
}

// TestIOPool_SlotFreedWhileRegistering: a slot freed after acquire found
// none but before it registered as waiting (its releaser found no one to
// pump) is taken, not left idle with the board waiting.
func TestIOPool_SlotFreedWhileRegistering(t *testing.T) {
	pool := newIOPool(1)
	pool.slots <- struct{}{} // another board's job holds the only slot
	b := newWorkflowBoard("/projects")
	pool.mu.Lock()
	got := make(chan bool, 1)
	go func() { got <- pool.acquire(b) }()
	testhelper.Eventually(t, func() bool { return acquireBlockedOnPoolMu(pool) }, 5*time.Second, "acquire never reached its registration")
	pool.release() // that job ends; its wakeWaiter finds no one waiting
	pool.mu.Unlock()
	if !<-got {
		t.Fatal("acquire missed the slot freed while it registered")
	}
	pool.mu.Lock()
	waiting := len(pool.waiting)
	pool.mu.Unlock()
	if waiting != 0 || len(pool.slots) != 1 {
		t.Errorf("%d boards waiting, %d slots taken; want 0 and 1", waiting, len(pool.slots))
	}
}

// TestWorkflowBoard_NilSafe: a nil board reads as a session without
// workflows through every method.
func TestWorkflowBoard_NilSafe(t *testing.T) {
	var b *WorkflowBoard
	ch, cancel := b.Subscribe()
	cancel()
	if ch != nil || b.Published() != nil || b.Summaries() != nil || b.Running() || b.LastObservedAt() != 0 || b.knownTaskIDs() != nil || b.refs(time.Now()) != nil {
		t.Error("nil board reported workflows")
	}
	if _, ok := b.WorkflowAgent("a1"); ok {
		t.Error("nil board found an agent")
	}
	if _, st := b.AgentTranscript("a1"); st != TranscriptNone {
		t.Errorf("nil board's transcript status %d, want TranscriptNone", st)
	}
	b.bind(&setProc{}, "/ws")
	b.procEnded(&setProc{}, cli.ProcessEnd{})
	b.restore("k", []workflow.Ref{{TaskID: "w1", Status: workflow.StatusRunning}}, "/ws", time.Now())
	b.setNotify(nil, nil)
	s := &ManagedSession{key: "k"}
	if s.workflowPinned(time.Now()) || s.snapshot(false).Workflows != nil {
		t.Error("a session without a board reported workflows")
	}
}

// TestWorkflowBoard_Subscribe: a wire change wakes every subscriber once
// (coalesced); a cancelled one no longer.
func TestWorkflowBoard_Subscribe(t *testing.T) {
	r := newWFRig(t)
	p := &setProc{}
	r.b.bind(p, "/ws")
	ch1, cancel1 := r.b.Subscribe()
	ch2, cancel2 := r.b.Subscribe()
	defer cancel2()
	p.publish(wfEntry("w1", workflow.StatusRunning))
	p.publish(wfEntry("w1", workflow.StatusCompleted))
	for i, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		default:
			t.Fatalf("subscriber %d not woken", i+1)
		}
	}
	cancel1()
	p.publish(wfEntry("w2", workflow.StatusRunning))
	select {
	case <-ch1:
		t.Error("a cancelled subscriber was woken")
	default:
	}
	<-ch2
}

// TestRestoredWorkflow: sessions.json is hand-editable. A non-empty session
// or run id must pass its check and the task id and status must be CC's;
// empty ids are kept (not known yet). Running entries come back stale.
func TestRestoredWorkflow(t *testing.T) {
	now := time.UnixMilli(wfT0)
	ok := workflow.Ref{TaskID: "w113pvmto", Status: workflow.StatusRunning}
	cases := []struct {
		name string
		mod  func(*workflow.Ref)
		keep bool
	}{
		{"empty ids", func(*workflow.Ref) {}, true},
		{"valid ids", func(r *workflow.Ref) { r.SessionID, r.RunID = wfSID, wfRun }, true},
		{"bad session id", func(r *workflow.Ref) { r.SessionID = "../x" }, false},
		{"bad run id", func(r *workflow.Ref) { r.RunID = "wf_../../x" }, false},
		{"bad task id", func(r *workflow.Ref) { r.TaskID = "W/1" }, false},
		{"unknown status", func(r *workflow.Ref) { r.Status = "exploded" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := ok
			tc.mod(&ref)
			w, kept := restoredWorkflow(ref, now)
			if kept != tc.keep {
				t.Fatalf("kept = %v, want %v", kept, tc.keep)
			}
			if kept && (w.Degraded != workflow.DegradedSnapshotStale || w.Source != workflow.SourceRef || w.LastObservedAt != wfT0) {
				t.Errorf("restored %+v: want snapshot_stale, source ref, observed at restore", w)
			}
		})
	}
}

// TestHeaderEqualCoversTheWire: headerEqual compares every WireView field
// but Agents and Version; a new wire field must be added there.
func TestHeaderEqualCoversTheWire(t *testing.T) {
	if n := reflect.TypeOf(workflow.WireView{}).NumField(); n != 21 {
		t.Fatalf("WireView has %d fields; add the new one to headerEqual and update this count", n)
	}
	base := wfEntry("w1", workflow.StatusRunning)
	base.Phases = []workflow.Phase{{Index: 1, Title: "Ask"}}
	muts := map[string]func(*workflow.Workflow){
		"RunID": func(w *workflow.Workflow) { w.RunID = "x" }, "Name": func(w *workflow.Workflow) { w.Name = "x" },
		"Description": func(w *workflow.Workflow) { w.Description = "x" }, "Current": func(w *workflow.Workflow) { w.Current = "x" },
		"Status": func(w *workflow.Workflow) { w.Status = workflow.StatusFailed }, "RawStatus": func(w *workflow.Workflow) { w.RawStatus = "x" },
		"StartedAt": func(w *workflow.Workflow) { w.StartedAt = 1 }, "EndedAt": func(w *workflow.Workflow) { w.EndedAt = 1 },
		"LastObservedAt": func(w *workflow.Workflow) { w.LastObservedAt = 1 }, "Tokens": func(w *workflow.Workflow) { w.Tokens = 1 },
		"ToolCalls": func(w *workflow.Workflow) { w.ToolCalls = 1 }, "DurationMS": func(w *workflow.Workflow) { w.DurationMS = 1 },
		"Counts": func(w *workflow.Workflow) { w.Counts.Done = 1 }, "Phases": func(w *workflow.Workflow) { w.Phases = []workflow.Phase{{Index: 1, Title: "B"}} },
		"AgentsCapped": func(w *workflow.Workflow) { w.AgentsCapped = true }, "NotifySummary": func(w *workflow.Workflow) { w.NotifySummary = "x" },
		"Source": func(w *workflow.Workflow) { w.Source = workflow.SourceReplay }, "Degraded": func(w *workflow.Workflow) { w.Degraded = "x" },
		"TaskID": func(w *workflow.Workflow) { w.TaskID = "w2" },
	}
	for name, mut := range muts {
		w := *base
		mut(&w)
		if headerEqual(&w, base) {
			t.Errorf("headerEqual misses %s", name)
		}
	}
	w := *base
	w.RunDir, w.SessionID, w.Version = "/x", wfSID, 9
	if !headerEqual(&w, base) {
		t.Error("headerEqual compares a field that is not on the wire")
	}
}
