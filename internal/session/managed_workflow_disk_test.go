package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
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

// fakeDisk is a projects root in memory: every run id resolves, result
// files are keyed by run id, agent transcripts name their run. Resolves and
// reads wait on their gate when one is set.
type fakeDisk struct {
	mu          sync.Mutex
	files       map[string]*workflow.ResultFile
	agents      map[string]string
	resolveGate chan struct{}
	readGate    chan struct{}

	// missing makes every resolution fail, a run dir not on disk yet.
	missing atomic.Bool

	resolves, reads, locates atomic.Int32
	inflight, peak           atomic.Int32
}

func newFakeDisk() *fakeDisk {
	return &fakeDisk{files: map[string]*workflow.ResultFile{}, agents: map[string]string{}}
}

func (f *fakeDisk) put(runID string, rf *workflow.ResultFile) {
	f.mu.Lock()
	f.files[runID] = rf
	f.mu.Unlock()
}

func (f *fakeDisk) disk() workflowDisk {
	resolveGate, readGate := f.resolveGate, f.readGate
	return workflowDisk{
		resolve: func(_ string, src workflowRunSource) (workflowRun, bool) {
			f.resolves.Add(1)
			if resolveGate != nil {
				<-resolveGate
			}
			if f.missing.Load() {
				return workflowRun{}, false
			}
			return workflowRun{RunDir: "/projects/s/" + src.RunID, ResultRel: src.RunID + ".json", SessionID: wfSID}, true
		},
		read: func(_ string, run workflowRun) (*workflow.ResultFile, error) {
			f.reads.Add(1)
			cur := f.inflight.Add(1)
			defer f.inflight.Add(-1)
			for {
				p := f.peak.Load()
				if cur <= p || f.peak.CompareAndSwap(p, cur) {
					break
				}
			}
			if readGate != nil {
				<-readGate
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if rf := f.files[strings.TrimSuffix(run.ResultRel, ".json")]; rf != nil {
				return rf, nil
			}
			return nil, os.ErrNotExist
		},
		locate: func(_, _, _ string, agents []string) (string, error) {
			f.locates.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, a := range agents {
				if run := f.agents[a]; run != "" {
					return run, nil
				}
			}
			return "", nil
		},
	}
}

// diskRig is a board rig on a fake disk.
func diskRig(t *testing.T, d *fakeDisk) *wfRig {
	t.Helper()
	r := newWFRig(t)
	r.b.disk = d.disk()
	return r
}

// settleIO waits until the board has no I/O queued or in flight.
func (r *wfRig) settleIO(t *testing.T) {
	t.Helper()
	testhelper.Eventually(t, func() bool {
		r.b.mu.Lock()
		defer r.b.mu.Unlock()
		return len(r.b.io.inflight) == 0 && len(r.b.io.queue) == 0
	}, 5*time.Second, "board I/O never drained")
}

func (r *wfRig) sweepAfter(t *testing.T, d time.Duration) {
	t.Helper()
	r.advance(d)
	r.b.sweep(r.now())
	r.settleIO(t)
}

// resultFile is the file CC writes when task's run completes: three done
// agents, totals, a result and a log line.
func resultFile(task string) *workflow.ResultFile {
	rf := &workflow.ResultFile{
		TaskID: task, Status: "completed", StartTime: wfT0 - 60_000, DurationMs: 50_000,
		TotalTokens: 1234, TotalToolCalls: 9, Result: json.RawMessage(`{"answer":"Paris"}`), Logs: []string{"phase 1 done"},
		WorkflowProgress: []clievent.WorkflowItem{{Type: clievent.WorkflowItemPhase, Index: 1, Title: "Ask"}},
	}
	for i := 1; i <= 3; i++ {
		rf.WorkflowProgress = append(rf.WorkflowProgress, clievent.WorkflowItem{
			Type: clievent.WorkflowItemAgent, Index: i, PhaseIndex: 1, Label: fmt.Sprintf("agent %d", i),
			State: "done", AgentID: fmt.Sprintf("a%016x", i), StartedAt: wfT0 - 50_000,
		})
	}
	return rf
}

func runningWithRun(id, runID string) *workflow.Workflow {
	w := wfEntry(id, workflow.StatusRunning, wfRow(1, workflow.AgentRunning))
	w.RunID, w.SessionID, w.Src.SessionID = runID, wfSID, workflow.SessionFromLaunch
	return w
}

// TestWorkflowSweep_LostTerminalFrame is PR-9's first acceptance: a run
// whose task_updated was lost is settled from its result file on the first
// sweep after 60s without an observation. The Tracker merges it, and the
// result cache is written before the merge publishes source=result_file.
func TestWorkflowSweep_LostTerminalFrame(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	p := &setProc{}
	p.onApply = func(*workflow.ResultFile) {
		if r.b.cachedResult("w1") == nil {
			t.Error("the Tracker merged before the result cache was written")
		}
	}
	p.publish(runningWithRun("w1", wfRun))
	r.b.bind(p, "/ws")
	r.settleIO(t)
	if n := d.reads.Load(); n != 1 {
		t.Fatalf("%d reads at bind, want 1: the reconnect reconciliation (R3b)", n)
	}
	d.put(wfRun, resultFile("w1")) // CC ended the run; its frame never arrived

	r.sweepAfter(t, 30*time.Second)
	if n := d.reads.Load(); n != 1 {
		t.Fatalf("%d reads 30s after the last observation, want no new one before 60s", n)
	}
	r.sweepAfter(t, 30*time.Second)
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").Status == workflow.StatusCompleted }, 5*time.Second, "the result file never settled the run")
	w := r.entry(t, "w1")
	if w.Source != workflow.SourceResultFile || len(w.Agents) != 3 || w.Tokens != 1234 {
		t.Errorf("after the merge: source %s, %d rows, %d tokens; want the file's", w.Source, len(w.Agents), w.Tokens)
	}
	if c := r.b.cachedResult("w1"); c == nil || c.Result != `{"answer":"Paris"}` || len(c.Logs) != 1 {
		t.Errorf("result cache %+v", c)
	}
	r.sweepAfter(t, 90*time.Second)
	if n := d.reads.Load(); n != 2 {
		t.Errorf("%d reads, want 2: a loaded result is never read again", n)
	}
}

// TestWorkflowSweep_ResumedRunOldFile: while a resumed run (same run id,
// new task id) runs, the file on disk is the old attempt's; it is not
// taken for the new one.
func TestWorkflowSweep_ResumedRunOldFile(t *testing.T) {
	d := newFakeDisk()
	d.put(wfRun, resultFile("wold"))
	r := diskRig(t, d)
	p := &setProc{}
	p.publish(runningWithRun("w1", wfRun))
	r.b.bind(p, "/ws")
	r.settleIO(t)
	r.sweepAfter(t, 61*time.Second)
	if n := d.reads.Load(); n != 2 {
		t.Fatalf("%d reads, want 2", n)
	}
	if w := r.entry(t, "w1"); w.Status != workflow.StatusRunning || r.b.cachedResult("w1") != nil {
		t.Errorf("status %s, cached %v; the old attempt's file must not settle the new run", w.Status, r.b.cachedResult("w1") != nil)
	}
}

// TestWorkflowSweep_TerminalRetry is §5.6(4): a run turning terminal reads
// its result file once; a file that is not there yet is retried every 30s
// for 10 minutes after the run ended, then left.
func TestWorkflowSweep_TerminalRetry(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	p := &setProc{}
	w := runningWithRun("w1", wfRun)
	p.publish(w)
	r.b.bind(p, "/ws")
	r.settleIO(t)
	done := *w
	done.Status, done.EndedAt = workflow.StatusCompleted, wfT0
	p.publish(&done)
	r.settleIO(t)
	if n := d.reads.Load(); n != 2 {
		t.Fatalf("%d reads, want the bind's and the terminal one", n)
	}
	d.put(wfRun, resultFile("w1"))
	r.sweepAfter(t, 29*time.Second)
	if n := d.reads.Load(); n != 2 {
		t.Fatalf("%d reads 29s on, want no retry before 30s", n)
	}
	r.sweepAfter(t, time.Second)
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").Source == workflow.SourceResultFile }, 5*time.Second, "the late file never merged")
	if got := r.entry(t, "w1"); len(got.Agents) != 3 || got.Tokens != 1234 {
		t.Errorf("merged %d rows, %d tokens", len(got.Agents), got.Tokens)
	}

	// Another run that never gets its file: retried until 10 minutes.
	other := runningWithRun("w2", "wf_bbbbbbbb-222")
	other.Status, other.EndedAt = workflow.StatusFailed, r.now().UnixMilli()
	p.publish(p.Workflows().Workflows[0], other)
	r.settleIO(t)
	base := d.reads.Load()
	for range 20 {
		r.sweepAfter(t, 30*time.Second)
	}
	if n := d.reads.Load() - base; n != 20 {
		t.Errorf("%d retries in 10 minutes, want 20", n)
	}
	for range 4 {
		r.sweepAfter(t, 30*time.Second)
	}
	if n := d.reads.Load() - base; n != 20 {
		t.Errorf("%d retries after 12 minutes, want still 20", n)
	}
}

// TestWorkflowSweep_OrphanInterrupted is R4: entries restored from
// sessions.json that no process takes up are interrupted 90s after the
// restore, once a read finds no result file (or at once without a run dir
// to read), and a CLI reporting the task later brings it back.
func TestWorkflowSweep_OrphanInterrupted(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	r.b.restore("k", []workflow.Ref{
		{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusRunning, LastObservedAt: wfT0},
		{TaskID: "w2", Status: workflow.StatusRunning, LastObservedAt: wfT0},
	}, "/ws", r.now())
	r.settleIO(t)
	r.sweepAfter(t, 60*time.Second)
	for _, id := range []string{"w1", "w2"} {
		if st := r.entry(t, id).Status; st != workflow.StatusRunning {
			t.Fatalf("%s %s at 60s, want running until 90s", id, st)
		}
	}
	r.sweepAfter(t, 30*time.Second)
	testhelper.Eventually(t, func() bool {
		return r.entry(t, "w1").Status == workflow.StatusInterrupted && r.entry(t, "w2").Status == workflow.StatusInterrupted
	}, 5*time.Second, "orphaned entries never interrupted")
	if got := r.entry(t, "w1"); got.EndedAt != r.now().UnixMilli() || r.b.Running() {
		t.Errorf("EndedAt %d, Running %v; want the sweep's time and nothing running", got.EndedAt, r.b.Running())
	}
	if n := d.reads.Load(); n != 2 {
		t.Errorf("%d reads, want 2 (the 60s one and R4's)", n)
	}

	p := &setProc{}
	p.publish(runningWithRun("w1", wfRun))
	r.b.bind(p, "/ws")
	if st := r.entry(t, "w1").Status; st != workflow.StatusRunning {
		t.Errorf("a reattached CLI reporting w1: %s, want running again", st)
	}
}

// TestWorkflowSweep_OrphanFileWins: R4's read finding a matching result
// file settles the entry from it, not as interrupted.
func TestWorkflowSweep_OrphanFileWins(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	r.b.restore("k", []workflow.Ref{{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusRunning, LastObservedAt: wfT0}}, "/ws", r.now())
	r.settleIO(t)
	r.sweepAfter(t, 60*time.Second)
	d.put(wfRun, resultFile("w1"))
	r.sweepAfter(t, 30*time.Second)
	if w := r.entry(t, "w1"); w.Status != workflow.StatusCompleted || len(w.Agents) != 3 || r.b.cachedResult("w1") == nil {
		t.Errorf("status %s, %d rows; want the file's completed run", w.Status, len(w.Agents))
	}
}

// TestWorkflowSweep_Unclaimed is R5: a restored running entry that the
// bound CLI never reports becomes unknown 90s after the bind (10 minutes
// when the bind's replay had wrapped): no longer pinning, still shown, and
// back to running when the CLI does report it.
func TestWorkflowSweep_Unclaimed(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%v", wrapped), func(t *testing.T) {
			d := newFakeDisk()
			r := diskRig(t, d)
			r.b.restore("k", []workflow.Ref{{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusRunning, LastObservedAt: wfT0}}, "/ws", r.now())
			p := &setProc{set: &workflow.Set{Version: 1, SeedWrapped: wrapped}}
			r.b.bind(p, "/ws")
			r.settleIO(t)
			window := workflowUnclaimedAfter
			if wrapped {
				window = workflowUnclaimedAfterWrapped
			}
			r.sweepAfter(t, window-time.Second)
			if st := r.entry(t, "w1").Status; st != workflow.StatusRunning {
				t.Fatalf("%s before the window, want running", st)
			}
			r.sweepAfter(t, time.Second)
			testhelper.Eventually(t, func() bool { return r.entry(t, "w1").Status == workflow.StatusUnknown }, 5*time.Second, "never unclaimed")
			w := r.entry(t, "w1")
			if w.RawStatus != workflow.RawStatusUnclaimed || w.Degraded != "" || r.b.Running() {
				t.Errorf("raw %q degraded %q running %v; want unclaimed, not stale, not running", w.RawStatus, w.Degraded, r.b.Running())
			}
			if sums := r.b.Summaries(); len(sums) != 1 || sums[0].Status != workflow.StatusUnknown {
				t.Errorf("summaries %+v; an unknown entry stays shown", sums)
			}
			p.publish(runningWithRun("w1", wfRun))
			if st := r.entry(t, "w1").Status; st != workflow.StatusRunning {
				t.Errorf("reported by the CLI: %s, want running", st)
			}
		})
	}
}

// TestWorkflowSweep_ClaimedNotUnclaimed: an entry the bound Tracker has
// reported is never R5's, even after it leaves the Tracker's Set.
func TestWorkflowSweep_ClaimedNotUnclaimed(t *testing.T) {
	r := diskRig(t, newFakeDisk())
	r.b.restore("k", []workflow.Ref{{TaskID: "w1", Status: workflow.StatusRunning, LastObservedAt: wfT0}}, "/ws", r.now())
	p := &setProc{}
	p.publish(wfEntry("w1", workflow.StatusRunning))
	r.b.bind(p, "/ws")
	p.publish() // the Tracker no longer holds it
	r.sweepAfter(t, 2*workflowUnclaimedAfter)
	if st := r.entry(t, "w1").Status; st != workflow.StatusRunning {
		t.Errorf("%s; a claimed entry must not be unclaimed", st)
	}
}

// TestWorkflowSweep_SkipsUnresolved: while an entry's run dir resolves,
// the sweeper neither reads nor settles it.
func TestWorkflowSweep_SkipsUnresolved(t *testing.T) {
	d := newFakeDisk()
	d.resolveGate = make(chan struct{})
	r := diskRig(t, d)
	r.b.restore("k", []workflow.Ref{{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusRunning, LastObservedAt: wfT0}}, "/ws", r.now())
	testhelper.Eventually(t, func() bool { return d.resolves.Load() == 1 }, 5*time.Second, "resolution never started")
	r.advance(2 * workflowOrphanAfter)
	r.b.sweep(r.now())
	if st := r.entry(t, "w1").Status; st != workflow.StatusRunning || d.reads.Load() != 0 {
		t.Fatalf("%s after %d reads while resolving; want running, untouched", st, d.reads.Load())
	}
	close(d.resolveGate)
	r.settleIO(t)
	r.sweepAfter(t, 0)
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").Status == workflow.StatusInterrupted }, 5*time.Second, "never swept once resolved")
}

// TestWorkflowBoard_RestoredTerminalBackfill is R0's terminal backfill: a
// restored terminal entry (a Ref has no rows) gets its rows, totals and
// result from one read once its run dir resolves, with no sweep, and the
// wire version moves. Without a file it is read once, not retried.
func TestWorkflowBoard_RestoredTerminalBackfill(t *testing.T) {
	d := newFakeDisk()
	d.put(wfRun, resultFile("w1"))
	r := diskRig(t, d)
	refs := []workflow.Ref{
		{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: wfT0 - 3_600_000, Counts: workflow.Counts{Total: 3, Done: 3}},
		{TaskID: "w2", RunID: "wf_bbbbbbbb-222", SessionID: wfSID, Status: workflow.StatusFailed, EndedAt: wfT0 - 3_600_000},
	}
	r.b.restore("k", refs, "/ws", r.now())
	v := r.entry(t, "w1").Version
	r.settleIO(t)
	testhelper.Eventually(t, func() bool { return len(r.entry(t, "w1").Agents) == 3 }, 5*time.Second, "restored terminal entry never got its rows")
	w := r.entry(t, "w1")
	if w.Source != workflow.SourceResultFile || w.Version <= v || w.Tokens != 1234 || r.b.cachedResult("w1") == nil {
		t.Errorf("source %s version %d (was %d) tokens %d; want the file merged and published", w.Source, w.Version, v, w.Tokens)
	}
	for range 4 {
		r.sweepAfter(t, 30*time.Second)
	}
	if n := d.reads.Load(); n != 2 {
		t.Errorf("%d reads, want one per entry: an old terminal entry is tried once", n)
	}
}

// TestWorkflowBoard_LocateRunByAgent is R3a: a live entry seeded from a
// wrapped replay without its run id finds it from its agents' transcripts,
// and the run dir then resolves. An unwrapped bind does not scan.
func TestWorkflowBoard_LocateRunByAgent(t *testing.T) {
	for _, wrapped := range []bool{true, false} {
		t.Run(fmt.Sprintf("wrapped=%v", wrapped), func(t *testing.T) {
			d := newFakeDisk()
			d.agents["a2093755b9a9ce8c0"] = wfRun
			r := diskRig(t, d)
			w := wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentQueued), wfRow(2, workflow.AgentRunning))
			w.Agents[1].AgentID, w.SessionID = "a2093755b9a9ce8c0", wfSID
			p := &setProc{set: &workflow.Set{Version: 1, SeedWrapped: wrapped, Workflows: []*workflow.Workflow{w}}}
			r.b.bind(p, "/ws")
			r.settleIO(t)
			got := r.entry(t, "w1")
			if !wrapped {
				if d.locates.Load() != 0 || got.RunID != "" {
					t.Errorf("unwrapped: %d scans, run id %q; want none", d.locates.Load(), got.RunID)
				}
				return
			}
			testhelper.Eventually(t, func() bool { return r.entry(t, "w1").RunDir != "" }, 5*time.Second, "located run never resolved")
			got = r.entry(t, "w1")
			if got.RunID != wfRun || d.locates.Load() != 1 {
				t.Errorf("run id %q after %d scans, want %s after one", got.RunID, d.locates.Load(), wfRun)
			}
			p.publish(w) // later frames: no second scan
			r.settleIO(t)
			if d.locates.Load() != 1 || r.entry(t, "w1").RunID != wfRun {
				t.Errorf("%d scans; the located run id must stay", d.locates.Load())
			}
		})
	}
}

// TestWorkflowBoard_BindReconciles is R3b: binding a process (a reattach
// after a restart) reads the result file of each entry with a run id, so a
// run that ended while naozhi was away settles without waiting for a sweep.
func TestWorkflowBoard_BindReconciles(t *testing.T) {
	d := newFakeDisk()
	d.put(wfRun, resultFile("w1"))
	r := diskRig(t, d)
	p := &setProc{}
	p.publish(runningWithRun("w1", wfRun))
	r.b.bind(p, "/ws")
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").Status == workflow.StatusCompleted }, 5*time.Second, "the bind's reconciliation never settled the run")
	r.b.bind(p, "/ws") // the same process again (rename): no new reconciliation
	r.settleIO(t)
	if n := d.reads.Load(); n != 1 {
		t.Errorf("%d reads, want 1", n)
	}
}

// TestWorkflowSweep_StuckReadsBounded: reads hung on a dead filesystem keep
// their slots; a board starts at most one more once one is stuck, and
// across boards at most workflowIOSlots are ever in flight, over 100 ticks.
func TestWorkflowSweep_StuckReadsBounded(t *testing.T) {
	d := newFakeDisk()
	d.readGate = make(chan struct{})
	t.Cleanup(func() { close(d.readGate) })
	pool := newIOPool(workflowIOSlots)
	var rigs []*wfRig
	for i := range 6 {
		r := diskRig(t, d)
		r.b.io.pool = pool
		var wfs []*workflow.Workflow
		for j := range 4 {
			wfs = append(wfs, runningWithRun(fmt.Sprintf("w%d", j), fmt.Sprintf("wf_%08d-%03d", i, j)))
		}
		p := &setProc{}
		p.publish(wfs...)
		r.b.bind(p, "/ws")
		rigs = append(rigs, r)
	}
	inflight := func(b *WorkflowBoard) int {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.io.inflight)
	}
	testhelper.Eventually(t, func() bool { return d.inflight.Load() == workflowIOSlots }, 5*time.Second, "slots never filled")
	for tick := range 100 {
		for _, r := range rigs {
			r.advance(30 * time.Second)
			r.b.sweep(r.now())
		}
		for i, r := range rigs {
			if n := inflight(r.b); n > workflowBoardIOInFlight+1 {
				t.Fatalf("tick %d: board %d has %d jobs in flight, want ≤ %d", tick, i, n, workflowBoardIOInFlight+1)
			}
		}
	}
	if p := d.peak.Load(); p > workflowIOSlots {
		t.Errorf("peak %d reads in flight, want ≤ %d", p, workflowIOSlots)
	}

	// One board alone: two in flight, then one more once they are stuck.
	solo := newFakeDisk()
	solo.readGate = make(chan struct{})
	r := diskRig(t, solo)
	var wfs []*workflow.Workflow
	for j := range 4 {
		wfs = append(wfs, runningWithRun(fmt.Sprintf("w%d", j), fmt.Sprintf("wf_%08d-%03d", 9, j)))
	}
	p := &setProc{}
	p.publish(wfs...)
	r.b.bind(p, "/ws")
	testhelper.Eventually(t, func() bool { return solo.inflight.Load() == workflowBoardIOInFlight }, 5*time.Second, "board reads never started")
	r.advance(workflowIOStuckAfter + time.Second)
	r.b.sweep(r.now())
	testhelper.Eventually(t, func() bool { return solo.inflight.Load() == workflowBoardIOInFlight+1 }, 5*time.Second, "no extra read once stuck")
	r.advance(workflowIOStuckAfter + time.Second)
	r.b.sweep(r.now())
	if n := inflight(r.b); n != workflowBoardIOInFlight+1 {
		t.Errorf("%d in flight with three stuck, want %d", n, workflowBoardIOInFlight+1)
	}
	close(solo.readGate)
	r.settleIO(t)
}

// TestWorkflowSweep_StaleReadDropped: a read still in flight when the
// entry's source changes returns into a state that is gone; its file does
// not merge.
func TestWorkflowSweep_StaleReadDropped(t *testing.T) {
	d := newFakeDisk()
	d.readGate = make(chan struct{})
	d.put(wfRun, resultFile("w1"))
	r := diskRig(t, d)
	p := &setProc{}
	w := runningWithRun("w1", wfRun)
	p.publish(w)
	r.b.bind(p, "/ws")
	testhelper.Eventually(t, func() bool { return d.inflight.Load() == 1 }, 5*time.Second, "read never started")
	moved := *w
	moved.RunID = "wf_bbbbbbbb-222"
	p.publish(&moved)
	close(d.readGate)
	r.settleIO(t)
	if got := r.entry(t, "w1"); got.Status != workflow.StatusRunning || r.b.cachedResult("w1") != nil {
		t.Errorf("status %s, cached %v; the stale read must be dropped", got.Status, r.b.cachedResult("w1") != nil)
	}
}

// TestSweepWorkflowBoards: the save tick's free function sweeps every
// session's board.
func TestSweepWorkflowBoards(t *testing.T) {
	r := wfRouter(t)
	for _, key := range []string{"feishu:direct:alice:general", "feishu:direct:bob:general"} {
		entry := &storeEntry{Key: key, Backend: "claude", Workspace: t.TempDir(), SessionID: wfSID,
			Workflows: []workflow.Ref{{TaskID: "w1", Status: workflow.StatusRunning, LastObservedAt: time.Now().UnixMilli()}}}
		r.ss.Update(func(tx sessTx) { r.restoreSessionFromEntry(tx, key, entry) })
	}
	sweepWorkflowBoards(r.ss, time.Now().Add(workflowOrphanAfter+time.Second))
	for _, key := range []string{"feishu:direct:alice:general", "feishu:direct:bob:general"} {
		if w := boardEntry(r.SessionFor(key), "w1"); w == nil || w.Status != workflow.StatusInterrupted {
			t.Errorf("%s: %+v, want interrupted by the sweep", key, w)
		}
	}
}

// TestWorkflowSweep_ObservedNotRead: an unsettled entry whose frames keep
// arriving is not looked up on disk, however long it runs.
func TestWorkflowSweep_ObservedNotRead(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	p := &setProc{}
	w := runningWithRun("w1", wfRun)
	p.publish(w)
	r.b.bind(p, "/ws")
	r.settleIO(t)
	for range 6 {
		r.advance(30 * time.Second)
		next := *w
		next.LastObservedAt = r.now().UnixMilli()
		p.publish(&next)
		r.sweepAfter(t, 0)
	}
	if n := d.reads.Load(); n != 1 {
		t.Errorf("%d reads, want only the bind's: a reporting run is not quiet", n)
	}
}

// TestWorkflowBoard_CapDropsCache: an entry the terminal cap drops takes
// its cached result with it.
func TestWorkflowBoard_CapDropsCache(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	var refs []workflow.Ref
	for i := range workflowBoardMaxTerminal {
		id, run := fmt.Sprintf("w%d", i), fmt.Sprintf("wf_%08d-000", i)
		d.put(run, resultFile(id))
		refs = append(refs, workflow.Ref{TaskID: id, RunID: run, SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: wfT0 + int64(i)})
	}
	r.b.restore("k", refs, "/ws", r.now())
	r.settleIO(t)
	testhelper.Eventually(t, func() bool { return r.b.cachedResult("w0") != nil }, 5*time.Second, "w0 never cached")
	r.b.restore("k", []workflow.Ref{{TaskID: "wnew", Status: workflow.StatusCompleted, EndedAt: wfT0 + 100}}, "/ws", r.now())
	if r.b.cachedResult("w0") != nil {
		t.Error("the dropped entry's result is still cached")
	}
	if got := taskIDs(r.pub()); len(got) != workflowBoardMaxTerminal || slices.Contains(got, "w0") {
		t.Errorf("published %v, want the 5 latest", got)
	}
}

// TestWorkflowSweep_ResolveRetried: CC can create the run dir just after
// the launch receipt arrives, so a failed resolution is tried again every
// 30s, and a run whose terminal frame was lost settles within 60s plus a
// tick of the dir appearing. Attempts stop 10 minutes after the first.
func TestWorkflowSweep_ResolveRetried(t *testing.T) {
	d := newFakeDisk()
	d.missing.Store(true)
	r := diskRig(t, d)
	p := &setProc{}
	p.publish(runningWithRun("w1", wfRun))
	r.b.bind(p, "/ws")
	r.settleIO(t)
	if n := d.resolves.Load(); n != 1 || r.entry(t, "w1").RunDir != "" {
		t.Fatalf("%d resolves, run dir %q; want one failed attempt", n, r.entry(t, "w1").RunDir)
	}
	d.missing.Store(false)
	d.put(wfRun, resultFile("w1"))
	r.sweepAfter(t, 29*time.Second)
	if n := d.resolves.Load(); n != 1 {
		t.Fatalf("%d resolves 29s on, want no retry before 30s", n)
	}
	r.sweepAfter(t, time.Second)
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").RunDir != "" }, 5*time.Second, "the retry never resolved the run dir")
	r.sweepAfter(t, 30*time.Second)
	r.sweepAfter(t, 30*time.Second)
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").Status == workflow.StatusCompleted }, 5*time.Second, "the lost terminal frame was never settled from the file")

	// A dir that never appears: retried every 30s for 10 minutes only.
	d.missing.Store(true)
	p.publish(p.Workflows().Workflows[0], runningWithRun("w2", "wf_bbbbbbbb-222"))
	r.settleIO(t)
	base := d.resolves.Load()
	for range 20 {
		r.sweepAfter(t, 30*time.Second)
	}
	if n := d.resolves.Load() - base; n != 20 {
		t.Errorf("%d retries in 10 minutes, want 20", n)
	}
	for range 4 {
		r.sweepAfter(t, 30*time.Second)
	}
	if n := d.resolves.Load() - base; n != 20 {
		t.Errorf("%d retries after 12 minutes, want still 20", n)
	}
}

// TestWorkflowSweep_ResolveRetryKeepsDebt: the reconciliation read a bind
// owes survives a failed first resolution, and is made once a retry finds
// the dir; the retry also finds a run the stream said nothing about.
func TestWorkflowSweep_ResolveRetryKeepsDebt(t *testing.T) {
	d := newFakeDisk()
	d.missing.Store(true)
	d.put(wfRun, resultFile("w1"))
	r := diskRig(t, d)
	p := &setProc{}
	p.publish(runningWithRun("w1", wfRun))
	r.b.bind(p, "/ws")
	r.settleIO(t)
	d.missing.Store(false)
	r.sweepAfter(t, 30*time.Second)
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").Status == workflow.StatusCompleted }, 5*time.Second, "the owed read never ran after the retry")
}

// TestWorkflowSweep_OrphanNotHeldByRetry: an entry whose run dir never
// resolves is still interrupted by R4 on time; the retry runs beside it.
func TestWorkflowSweep_OrphanNotHeldByRetry(t *testing.T) {
	d := newFakeDisk()
	d.missing.Store(true)
	r := diskRig(t, d)
	r.b.restore("k", []workflow.Ref{{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusRunning, LastObservedAt: wfT0}}, "/ws", r.now())
	r.settleIO(t)
	for range 4 {
		r.sweepAfter(t, 30*time.Second)
	}
	if st := r.entry(t, "w1").Status; st != workflow.StatusInterrupted {
		t.Errorf("status %s, want interrupted despite the retries", st)
	}
	if n := d.resolves.Load(); n < 3 {
		t.Errorf("%d resolves, want retries while interrupted", n)
	}
	d.missing.Store(false)
	d.put(wfRun, resultFile("w1"))
	r.sweepAfter(t, 30*time.Second)
	testhelper.Eventually(t, func() bool { return r.entry(t, "w1").Status == workflow.StatusCompleted }, 5*time.Second, "a late dir and file never overrode the orphan verdict")
}

// TestWorkflowBoard_LocateOncePerBind: a run dir scan that finds nothing
// is not repeated by later publications of the same agents, and an entry
// without a session id waits for one before scanning.
func TestWorkflowBoard_LocateOncePerBind(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	w := wfEntry("w1", workflow.StatusRunning, wfRow(1, workflow.AgentRunning))
	w.Agents[0].AgentID = "a2093755b9a9ce8c0"
	p := &setProc{set: &workflow.Set{Version: 1, SeedWrapped: true, Workflows: []*workflow.Workflow{w}}}
	r.b.bind(p, "/ws")
	r.settleIO(t)
	if n := d.locates.Load(); n != 0 {
		t.Fatalf("%d scans without a session id, want none", n)
	}
	w2 := *w
	w2.SessionID = wfSID
	p.publish(&w2)
	r.settleIO(t)
	for range 5 {
		w3 := w2
		w3.LastObservedAt++
		p.publish(&w3)
		r.settleIO(t)
	}
	if n := d.locates.Load(); n != 1 {
		t.Errorf("%d scans for an agent not on disk, want one per bind", n)
	}
}

// TestCleanupLoop_SweepsWorkflowBoards: the cleanup loop's save tick runs
// the workflow sweep, so an orphan restored from sessions.json is
// interrupted without anyone calling the sweeper by hand.
func TestCleanupLoop_SweepsWorkflowBoards(t *testing.T) {
	old := saveTickInterval
	saveTickInterval = 10 * time.Millisecond
	t.Cleanup(func() { saveTickInterval = old })
	r := wfRouter(t)
	const key = "feishu:direct:alice:general"
	entry := &storeEntry{Key: key, Backend: "claude", Workspace: t.TempDir(), SessionID: wfSID,
		Workflows: []workflow.Ref{{TaskID: "w1", Status: workflow.StatusRunning, LastObservedAt: time.Now().UnixMilli()}}}
	r.ss.Update(func(tx sessTx) { r.restoreSessionFromEntry(tx, key, entry) })
	b := r.SessionFor(key).WorkflowBoard()
	b.mu.Lock()
	b.procGone -= 2 * workflowOrphanAfter.Milliseconds()
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.StartCleanupLoop(ctx, time.Hour)
	testhelper.Eventually(t, func() bool {
		w := boardEntry(r.SessionFor(key), "w1")
		return w != nil && w.Status == workflow.StatusInterrupted
	}, 5*time.Second, "the save tick never swept the board")
}

// TestWorkflowSweep_RacesBind: the sweeper runs while the board is bound,
// published to and unbound over and over (RFC §11.3); under -race nothing
// trips and the board still answers afterwards.
func TestWorkflowSweep_RacesBind(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for at := r.now(); ; at = at.Add(45 * time.Second) {
			select {
			case <-stop:
				return
			default:
				r.b.sweep(at)
			}
		}
	}()
	for i := range 200 {
		p := &setProc{}
		p.publish(runningWithRun("w1", wfRun), runningWithRun("w2", "wf_bbbbbbbb-222"))
		r.b.bind(p, "/ws")
		if i%2 == 0 {
			p.publish(runningWithRun("w1", wfRun))
		}
		r.b.procEnded(p, cli.ProcessEnd{ShimLive: i%3 == 0})
	}
	close(stop)
	<-done
	r.settleIO(t)
	if r.b.Published() == nil {
		t.Error("no publication after the race")
	}
}
