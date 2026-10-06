package session

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// restoredEnded restores an ended task whose first read (at bind) found no
// file, the shape Result is for: resolved, terminal, without a cache.
func restoredEnded(t *testing.T, d *fakeDisk) *wfRig {
	t.Helper()
	r := diskRig(t, d)
	r.b.restore("k", []workflow.Ref{{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: wfT0 - 60_000}}, "/ws", r.now())
	r.settleIO(t)
	if n := d.reads.Load(); n != 1 {
		t.Fatalf("%d reads at restore, want the one backfill", n)
	}
	return r
}

// TestWorkflowBoard_ResultMergesOnce is §6.2.1's cache-miss path: the file
// is read once, its rows, totals and cache are merged and published before
// Result returns, and later calls hit the cache.
func TestWorkflowBoard_ResultMergesOnce(t *testing.T) {
	d := newFakeDisk()
	r := restoredEnded(t, d)
	v := r.entry(t, "w1").Version
	d.put(wfRun, resultFile("w1"))

	c, st := r.b.Result(context.Background(), "w1")
	if st != ResultReady || c == nil || c.Result != `{"answer":"Paris"}` || len(c.Logs) != 1 {
		t.Fatalf("Result = %+v, %v", c, st)
	}
	if w := r.entry(t, "w1"); len(w.Agents) != 3 || w.Version <= v || w.Source != workflow.SourceResultFile || w.Tokens != 1234 {
		t.Errorf("published after Result: %d rows, version %d (was %d), source %s, tokens %d; want the file merged", len(w.Agents), w.Version, v, w.Source, w.Tokens)
	}
	for range 3 {
		if c2, st := r.b.Result(context.Background(), "w1"); st != ResultReady || c2 != c {
			t.Fatalf("second Result = %p, %v; want the cached %p", c2, st, c)
		}
	}
	if n := d.reads.Load(); n != 2 {
		t.Errorf("%d reads, want the backfill and one Result read", n)
	}
	r.sweepAfter(t, 90*time.Second)
	if n := d.reads.Load(); n != 2 {
		t.Errorf("%d reads after a sweep, want a loaded result never read again", n)
	}
}

// TestWorkflowBoard_ResultStatuses is every answer that is not a result.
func TestWorkflowBoard_ResultStatuses(t *testing.T) {
	ctx := context.Background()
	var nilBoard *WorkflowBoard
	if c, st := nilBoard.Result(ctx, "w1"); c != nil || st != ResultNone {
		t.Errorf("nil board: %v, %v", c, st)
	}

	d := newFakeDisk()
	r := diskRig(t, d)
	p := &setProc{}
	p.publish(runningWithRun("w1", wfRun))
	r.b.bind(p, "/ws")
	r.settleIO(t)
	if _, st := r.b.Result(ctx, "w1"); st != ResultNone {
		t.Errorf("running task: %v, want ResultNone", st)
	}
	if _, st := r.b.Result(ctx, "nope"); st != ResultNone {
		t.Errorf("unknown task: %v, want ResultNone", st)
	}

	// Ended without a run id: no file can exist.
	r.b.restore("k", []workflow.Ref{{TaskID: "w2", SessionID: wfSID, Status: workflow.StatusKilled, EndedAt: wfT0}}, "/ws", r.now())
	if _, st := r.b.Result(ctx, "w2"); st != ResultNone {
		t.Errorf("no run id: %v, want ResultNone", st)
	}

	// Ended with a run id and no file, and one whose file names another task.
	r.b.restore("k", []workflow.Ref{
		{TaskID: "w3", RunID: "wf_cccccccc-333", SessionID: wfSID, Status: workflow.StatusFailed, EndedAt: wfT0},
		{TaskID: "w4", RunID: "wf_dddddddd-444", SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: wfT0},
	}, "/ws", r.now())
	d.put("wf_dddddddd-444", resultFile("wother"))
	r.settleIO(t)
	for _, id := range []string{"w3", "w4"} {
		before := d.reads.Load()
		if c, st := r.b.Result(ctx, id); c != nil || st != ResultUnavailable {
			t.Errorf("%s: %v, %v; want ResultUnavailable", id, c, st)
		}
		if d.reads.Load() != before+1 || r.b.cachedResult(id) != nil || r.entry(t, id).Source == workflow.SourceResultFile {
			t.Errorf("%s: reads %d (was %d), cached %v; want one read and nothing merged", id, d.reads.Load(), before, r.b.cachedResult(id) != nil)
		}
	}
}

// TestWorkflowBoard_ResultUnresolved: with the run dir not resolved (still
// resolving, or failed) there is no path to read, and Result does not try.
func TestWorkflowBoard_ResultUnresolved(t *testing.T) {
	d := newFakeDisk()
	d.resolveGate = make(chan struct{})
	r := diskRig(t, d)
	r.b.restore("k", []workflow.Ref{{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: wfT0}}, "/ws", r.now())
	if _, st := r.b.Result(context.Background(), "w1"); st != ResultUnavailable {
		t.Errorf("resolving: %v, want ResultUnavailable", st)
	}
	d.missing.Store(true)
	close(d.resolveGate)
	r.settleIO(t)
	if _, st := r.b.Result(context.Background(), "w1"); st != ResultUnavailable {
		t.Errorf("failed resolution: %v, want ResultUnavailable", st)
	}
	if n := d.reads.Load(); n != 0 {
		t.Errorf("%d reads without a run dir", n)
	}
}

// TestWorkflowBoard_ResultSingleflight: callers arriving during a read
// share it, whatever their number.
func TestWorkflowBoard_ResultSingleflight(t *testing.T) {
	d := newFakeDisk()
	r := restoredEnded(t, d)
	d.put(wfRun, resultFile("w1"))
	gate := make(chan struct{})
	r.b.disk.read = func(root string, run workflowRun) (*workflow.ResultFile, error) {
		<-gate
		return d.disk().read(root, run)
	}
	before := d.reads.Load()

	const callers = 12
	var wg sync.WaitGroup
	got := make([]ResultStatus, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, got[i] = r.b.Result(context.Background(), "w1")
		}()
	}
	testhelper.Eventually(t, func() bool {
		r.b.mu.Lock()
		defer r.b.mu.Unlock()
		return r.b.resultWait["w1"] != nil
	}, 5*time.Second, "no read started")
	close(gate)
	wg.Wait()
	for i, st := range got {
		if st != ResultReady {
			t.Errorf("caller %d: %v", i, st)
		}
	}
	if n := d.reads.Load() - before; n != 1 {
		t.Errorf("%d reads for %d callers, want 1", n, callers)
	}
	if len(r.b.resultWait) != 0 || len(r.b.io.pool.slots) != 0 {
		t.Errorf("%d waits and %d slots left behind", len(r.b.resultWait), len(r.b.io.pool.slots))
	}
}

// TestWorkflowBoard_ResultNoSlot: with every global slot taken Result
// answers at once, starts nothing and queues no wake-up for itself.
func TestWorkflowBoard_ResultNoSlot(t *testing.T) {
	d := newFakeDisk()
	r := restoredEnded(t, d)
	d.put(wfRun, resultFile("w1"))
	for range cap(r.b.io.pool.slots) {
		r.b.io.pool.slots <- struct{}{}
	}
	before := d.reads.Load()
	start := time.Now()
	if c, st := r.b.Result(context.Background(), "w1"); c != nil || st != ResultUnavailable {
		t.Fatalf("Result = %v, %v; want ResultUnavailable", c, st)
	}
	if time.Since(start) > time.Second || d.reads.Load() != before || len(r.b.io.pool.waiting) != 0 {
		t.Errorf("took %v, %d new reads, %d boards waiting; want an instant refusal", time.Since(start), d.reads.Load()-before, len(r.b.io.pool.waiting))
	}
	for range cap(r.b.io.pool.slots) {
		<-r.b.io.pool.slots
	}
	if _, st := r.b.Result(context.Background(), "w1"); st != ResultReady {
		t.Errorf("with a slot free again: %v, want ResultReady", st)
	}
}

// TestWorkflowBoard_ResultContext: a caller that gives up leaves the read
// running; it lands, and the next call finds it cached.
func TestWorkflowBoard_ResultContext(t *testing.T) {
	d := newFakeDisk()
	r := restoredEnded(t, d)
	d.put(wfRun, resultFile("w1"))
	gate := make(chan struct{})
	r.b.disk.read = func(root string, run workflowRun) (*workflow.ResultFile, error) {
		<-gate
		return d.disk().read(root, run)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if c, st := r.b.Result(ctx, "w1"); c != nil || st != ResultUnavailable {
		t.Fatalf("Result under an expired context = %v, %v", c, st)
	}
	close(gate)
	testhelper.Eventually(t, func() bool { return r.b.cachedResult("w1") != nil }, 5*time.Second, "the abandoned read never landed")
	if _, st := r.b.Result(context.Background(), "w1"); st != ResultReady {
		t.Errorf("after the read landed: %v", st)
	}
}

// TestWorkflowBoard_ResultLiveEntry: for an ended entry the Tracker still
// holds, the Tracker merges the file (after the cache is written), and
// Result returns once that is published.
func TestWorkflowBoard_ResultLiveEntry(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	p := &setProc{}
	ended := runningWithRun("w1", wfRun)
	ended.Status, ended.EndedAt, ended.LastObservedAt = workflow.StatusCompleted, wfT0, wfT0
	p.publish(ended)
	r.b.bind(p, "/ws")
	r.settleIO(t)
	d.put(wfRun, resultFile("w1"))
	var cachedFirst atomic.Bool
	var merges atomic.Int32
	p.onApply = func(*workflow.ResultFile) {
		cachedFirst.Store(r.b.cachedResult("w1") != nil)
		merges.Add(1)
	}

	if c, st := r.b.Result(context.Background(), "w1"); st != ResultReady || c == nil {
		t.Fatalf("Result = %v, %v", c, st)
	}
	if !cachedFirst.Load() || merges.Load() != 1 {
		t.Errorf("cache before the Tracker merge = %v, merges = %d; want true, 1", cachedFirst.Load(), merges.Load())
	}
	if w := r.entry(t, "w1"); w.Source != workflow.SourceResultFile || len(w.Agents) != 3 {
		t.Errorf("source %s, %d rows after Result", w.Source, len(w.Agents))
	}
}

// TestWorkflowBoard_ResultPanicHandsSlotBack: a reader that panics costs
// neither the slot nor the singleflight entry.
func TestWorkflowBoard_ResultPanicHandsSlotBack(t *testing.T) {
	d := newFakeDisk()
	r := restoredEnded(t, d)
	r.b.disk.read = func(string, workflowRun) (*workflow.ResultFile, error) { panic("boom") }
	if c, st := r.b.Result(context.Background(), "w1"); c != nil || st != ResultUnavailable {
		t.Fatalf("Result = %v, %v", c, st)
	}
	if len(r.b.resultWait) != 0 || len(r.b.io.pool.slots) != 0 {
		t.Errorf("%d waits and %d slots left behind", len(r.b.resultWait), len(r.b.io.pool.slots))
	}
}

// TestWorkflowBoard_ResultLiveEntryOtherTasksFile: the file under a live
// entry's run id names another task (a resumed run's earlier attempt); it
// is neither cached nor merged, however the Tracker would have reacted.
func TestWorkflowBoard_ResultLiveEntryOtherTasksFile(t *testing.T) {
	d := newFakeDisk()
	r := diskRig(t, d)
	p := &setProc{}
	ended := runningWithRun("w1", wfRun)
	ended.Status, ended.EndedAt = workflow.StatusCompleted, wfT0
	p.publish(ended)
	r.b.bind(p, "/ws")
	r.settleIO(t)
	d.put(wfRun, resultFile("wold"))
	if c, st := r.b.Result(context.Background(), "w1"); c != nil || st != ResultUnavailable {
		t.Fatalf("Result = %v, %v; want ResultUnavailable", c, st)
	}
	if r.b.cachedResult("w1") != nil || r.entry(t, "w1").Source == workflow.SourceResultFile {
		t.Error("another task's file was cached or merged")
	}
}

// TestWorkflowBoard_ResultReadWakesWaiter: a board that queued for a slot
// while a Result read held the last one is pumped when that read ends.
func TestWorkflowBoard_ResultReadWakesWaiter(t *testing.T) {
	pool := newIOPool(1)
	d := newFakeDisk()
	r := diskRig(t, d)
	r.b.io.pool = pool
	r.b.restore("k", []workflow.Ref{{TaskID: "w1", RunID: wfRun, SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: wfT0 - 60_000}}, "/ws", r.now())
	r.settleIO(t)
	d.put(wfRun, resultFile("w1"))
	gate := make(chan struct{})
	r.b.disk.read = func(root string, run workflowRun) (*workflow.ResultFile, error) {
		<-gate
		return d.disk().read(root, run)
	}
	got := make(chan ResultStatus, 1)
	go func() {
		_, st := r.b.Result(context.Background(), "w1")
		got <- st
	}()
	testhelper.Eventually(t, func() bool {
		r.b.mu.Lock()
		defer r.b.mu.Unlock()
		return r.b.resultWait["w1"] != nil
	}, 5*time.Second, "no read started")

	other := newFakeDisk()
	r2 := diskRig(t, other)
	r2.b.io.pool = pool
	p := &setProc{}
	p.publish(runningWithRun("w1", "wf_eeeeeeee-555"))
	r2.b.bind(p, "/ws")
	pool.mu.Lock()
	waiting := len(pool.waiting)
	pool.mu.Unlock()
	if waiting != 1 || other.resolves.Load() != 0 {
		t.Fatalf("%d boards waiting, %d resolves; want the second board queued behind the Result read", waiting, other.resolves.Load())
	}
	close(gate)
	if st := <-got; st != ResultReady {
		t.Fatalf("Result = %v", st)
	}
	testhelper.Eventually(t, func() bool { return other.resolves.Load() == 1 }, 5*time.Second, "the waiting board was never pumped")
}

// TestWorkflowBoard_ResultReadCountsForSweep: a Result read that found no
// file is the sweeper's latest read too; its next comes 30s after it.
func TestWorkflowBoard_ResultReadCountsForSweep(t *testing.T) {
	d := newFakeDisk()
	r := restoredEnded(t, d)
	r.advance(25 * time.Second)
	if _, st := r.b.Result(context.Background(), "w1"); st != ResultUnavailable {
		t.Fatalf("Result = %v, want ResultUnavailable", st)
	}
	r.sweepAfter(t, 6*time.Second) // 31s after the backfill, 6s after Result's read
	if n := d.reads.Load(); n != 2 {
		t.Errorf("%d reads, want the backfill and Result's: no sweep read 6s after Result's", n)
	}
	r.sweepAfter(t, 25*time.Second)
	if n := d.reads.Load(); n != 3 {
		t.Errorf("%d reads, want a sweep read 31s after Result's", n)
	}
}

// TestWorkflowBoard_ResultWaitsForTrackerMerge: a read of a live entry's
// file writes the cache before the Tracker merges it; a Result call in
// between waits for the merge rather than serving the cache next to rows
// not yet published, whether a sweep or another Result call read the file.
func TestWorkflowBoard_ResultWaitsForTrackerMerge(t *testing.T) {
	for _, viaSweep := range []bool{true, false} {
		name := "read by Result"
		if viaSweep {
			name = "read by the sweeper"
		}
		t.Run(name, func(t *testing.T) {
			d := newFakeDisk()
			r := diskRig(t, d)
			p := &setProc{}
			ended := runningWithRun("w1", wfRun)
			ended.Status, ended.EndedAt, ended.LastObservedAt = workflow.StatusCompleted, wfT0, wfT0
			p.publish(ended)
			r.b.bind(p, "/ws")
			r.settleIO(t)
			d.put(wfRun, resultFile("w1"))
			entered, gate := make(chan struct{}), make(chan struct{})
			p.onApply = func(*workflow.ResultFile) {
				close(entered)
				<-gate
			}
			first := make(chan ResultStatus, 1)
			if viaSweep {
				r.advance(31 * time.Second)
				r.b.sweep(r.now())
			} else {
				go func() {
					_, st := r.b.Result(context.Background(), "w1")
					first <- st
				}()
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the Tracker merge never started")
			}
			if r.b.cachedResult("w1") == nil {
				t.Fatal("no cache before the Tracker merge")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, st := r.b.Result(ctx, "w1")
			cancel()
			if st != ResultUnavailable {
				t.Errorf("Result during the Tracker merge = %v, want it to wait for the merge", st)
			}
			close(gate)
			if c, st := r.b.Result(context.Background(), "w1"); st != ResultReady || c == nil {
				t.Fatalf("Result after the merge = %v, %v", c, st)
			}
			if w := r.entry(t, "w1"); w.Source != workflow.SourceResultFile || len(w.Agents) != 3 {
				t.Errorf("source %s, %d rows when Result returned; want the merge published", w.Source, len(w.Agents))
			}
			if !viaSweep {
				if st := <-first; st != ResultReady {
					t.Errorf("the reading Result call = %v", st)
				}
			}
			r.settleIO(t)
			r.b.mu.Lock()
			left := len(r.b.resultWait)
			r.b.mu.Unlock()
			if left != 0 {
				t.Errorf("%d waits left behind", left)
			}
		})
	}
}
