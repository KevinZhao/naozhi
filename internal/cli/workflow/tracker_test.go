package workflow

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

var t0 = time.UnixMilli(1_791_000_000_000)

func started(id, name string) *clievent.Event {
	return &clievent.Event{Type: "system", SubType: "task_started", TaskID: id, TaskType: clievent.TaskTypeWorkflow, WorkflowName: name, Description: name + " desc", SessionID: "sid-launch"}
}

func progress(id string, items ...clievent.WorkflowItem) *clievent.Event {
	if items == nil {
		items = []clievent.WorkflowItem{}
	}
	return &clievent.Event{Type: "system", SubType: "task_progress", TaskID: id, WorkflowProgress: items, SessionID: "sid-progress"}
}

func header(id, desc string) *clievent.Event {
	return &clievent.Event{Type: "system", SubType: "task_progress", TaskID: id, Description: desc, TaskSummary: "sum", Usage: &clievent.TaskUsage{TotalTokens: 7, ToolUses: 2, DurationMS: 9}}
}

func updated(id, status string, end int64) *clievent.Event {
	return &clievent.Event{Type: "system", SubType: "task_updated", TaskID: id, Patch: &clievent.TaskPatch{Status: status, EndTime: end}}
}

func notification(id, status string) *clievent.Event {
	return &clievent.Event{Type: "system", SubType: "task_notification", TaskID: id, Status: status, TaskSummary: "note", Usage: &clievent.TaskUsage{TotalTokens: 100, ToolUses: 3, DurationMS: 50}}
}

func running(idx int, id string) clievent.WorkflowItem {
	return clievent.WorkflowItem{Type: clievent.WorkflowItemAgent, Index: idx, PhaseIndex: 1, Label: fmt.Sprint("L", idx), AgentID: id, State: "progress", StartedAt: 10, QueuedAt: 5}
}

func phase(idx int, title string) clievent.WorkflowItem {
	return clievent.WorkflowItem{Type: clievent.WorkflowItemPhase, Index: idx, Title: title}
}

func get(t *testing.T, tr *Tracker, id string) *Workflow {
	t.Helper()
	for _, w := range tr.Load().Workflows {
		if w.TaskID == id {
			return w
		}
	}
	t.Fatalf("no entry for %s in %+v", id, tr.Load().Workflows)
	return nil
}

func observeAll(tr *Tracker, at time.Time, evs ...*clievent.Event) {
	for _, ev := range evs {
		tr.Observe(ev, at)
	}
}

func TestLifecycle_TerminalStopsLiveRowsAndIgnoresLaterProgress(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	queued := clievent.WorkflowItem{Type: clievent.WorkflowItemAgent, Index: 3, PhaseIndex: 1, State: "start", QueuedAt: 5}
	done := clievent.WorkflowItem{Type: clievent.WorkflowItemAgent, Index: 2, PhaseIndex: 1, State: "done", AgentID: "a2", StartedAt: 6}
	observeAll(tr, t0, started("w1", "wf"), progress("w1", phase(1, "P"), running(1, "a1"), done, queued))
	w := get(t, tr, "w1")
	if w.Counts != (Counts{Total: 3, Queued: 1, Running: 1, Done: 1}) {
		t.Fatalf("counts %+v", w.Counts)
	}
	tr.Observe(updated("w1", "failed", 4242), t0.Add(time.Second))
	w = get(t, tr, "w1")
	if w.Status != StatusFailed || w.EndedAt != 4242 {
		t.Fatalf("task_updated: %s ended %d", w.Status, w.EndedAt)
	}
	states := []AgentState{w.Agents[0].State, w.Agents[1].State, w.Agents[2].State}
	if fmt.Sprint(states) != "[stopped done stopped]" || w.Counts != (Counts{Total: 3, Done: 1, Stopped: 2}) ||
		w.Phases[0].Counts != w.Counts {
		t.Fatalf("terminal rows %v counts %+v phase %+v", states, w.Counts, w.Phases[0].Counts)
	}
	before := w
	tr.Observe(progress("w1", running(1, "a1"), running(4, "a4")), t0.Add(2*time.Second))
	if get(t, tr, "w1") != before {
		t.Fatal("a task_progress after the terminal state changed the entry")
	}
	tr.Observe(notification("w1", "completed"), t0.Add(3*time.Second))
	w = get(t, tr, "w1")
	if w.Status != StatusFailed || w.NotifySummary != "note" || w.Tokens != 100 || w.EndedAt != 4242 {
		t.Fatalf("notification after task_updated: %+v", *w)
	}
}

func TestLifecycle_NotificationAlone(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	observeAll(tr, t0, started("w1", "wf"), notification("w1", "stopped"))
	w := get(t, tr, "w1")
	if w.Status != StatusKilled || w.EndedAt != t0.UnixMilli() || w.NotifySummary != "note" || w.DurationMS != 50 {
		t.Fatalf("stopped notification: %+v", *w)
	}
}

func TestLifecycle_HeaderOnlyFrameKeepsRows(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	observeAll(tr, t0, started("w1", "wf"), progress("w1", running(1, "a1")))
	rows := get(t, tr, "w1").Agents
	tr.Observe(header("w1", "P: L1"), t0)
	w := get(t, tr, "w1")
	if &w.Agents[0] != &rows[0] || w.Current != "P: L1" || w.Description != "sum" || w.Tokens != 7 || w.ToolCalls != 2 || w.DurationMS != 9 {
		t.Fatalf("description-only frame: shared rows %v, %+v", &w.Agents[0] == &rows[0], *w)
	}
	tr.Observe(&clievent.Event{Type: "system", SubType: "task_progress", TaskID: "w1"}, t0) // "workflow_progress":null decodes to nil
	if w := get(t, tr, "w1"); len(w.Agents) != 1 || w.SnapshotSeq != 1 {
		t.Fatalf("null snapshot cleared the rows: %+v", *w)
	}
}

func TestLifecycle_StickyAgentIDAndAttempts(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	tr.Observe(progress("w1", running(1, "a1")), t0)
	requeued := clievent.WorkflowItem{Type: clievent.WorkflowItemAgent, Index: 1, PhaseIndex: 1, State: "start", QueuedAt: 20}
	tr.Observe(progress("w1", requeued), t0)
	a := get(t, tr, "w1").Agents[0]
	if a.State != AgentQueued || a.AgentID != "a1" || a.PrevAgentIDs != nil {
		t.Fatalf("rate-limit requeue: %+v", a)
	}
	retry := running(1, "a9")
	retry.Attempt = 2
	tr.Observe(progress("w1", retry), t0)
	a = get(t, tr, "w1").Agents[0]
	if a.State != AgentRunning || a.AgentID != "a9" || fmt.Sprint(a.PrevAgentIDs) != "[a1]" || a.Attempt != 2 {
		t.Fatalf("retry: %+v", a)
	}
}

func TestLifecycle_UnknownStatusAndStatePassThrough(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	odd := running(1, "a1")
	odd.State = "hibernating"
	observeAll(tr, t0, progress("w1", odd), updated("w1", "adopted", 0))
	w := get(t, tr, "w1")
	if w.Status != StatusUnknown || w.RawStatus != "adopted" || w.Agents[0].State != AgentUnknown || w.Agents[0].RawState != "hibernating" {
		t.Fatalf("pass-through: %+v", *w)
	}
	if !IsUnsettled(w.Status) || IsRunning(w.Status) {
		t.Fatal("unknown must be unsettled and not running")
	}
	tr.Observe(updated("w1", "paused", 0), t0)
	if w := get(t, tr, "w1"); w.Status != StatusPaused || w.RawStatus != "" || !IsRunning(w.Status) {
		t.Fatalf("paused: %+v", *w)
	}
}

func TestLifecycle_InterleavedWorkflows(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	observeAll(tr, t0,
		started("w1", "one"), started("w2", "two"),
		progress("w1", running(1, "a1")), progress("w2", running(1, "b1"), running(2, "b2")),
		updated("w2", "completed", 99), header("w1", "P: x"))
	w1, w2 := get(t, tr, "w1"), get(t, tr, "w2")
	if w1.Name != "one" || w1.Status != StatusRunning || len(w1.Agents) != 1 || w1.Current != "P: x" {
		t.Errorf("w1: %+v", *w1)
	}
	if w2.Name != "two" || w2.Status != StatusCompleted || len(w2.Agents) != 2 || w2.Current != "" {
		t.Errorf("w2: %+v", *w2)
	}
	if wfs := tr.Load().Workflows; wfs[0] != w1 || wfs[1] != w2 {
		t.Errorf("order: running first, then terminal")
	}
}

// TestFieldSources covers the StartedAt, SessionID and Name grades the
// Tracker can assign (RFC §4.2).
func TestFieldSources(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	// Only progress frames seen: the snapshot's earliest time, the first
	// frame's session id, the summary as the name.
	ev := progress("w1", running(1, "a1"))
	ev.TaskSummary = "fallback"
	tr.Observe(ev, t0)
	w := get(t, tr, "w1")
	if w.StartedAt != 5 || w.SessionID != "sid-progress" || w.Name != "fallback" ||
		w.Src != (FieldSrc{Name: NameFromSummary, StartedAt: StartedFromSnapshot, SessionID: SessionFromProgress}) {
		t.Fatalf("progress only: %+v src %+v", *w, w.Src)
	}
	later := progress("w1", running(1, "a1"))
	later.SessionID = "sid-later"
	tr.Observe(later, t0)
	if w := get(t, tr, "w1"); w.SessionID != "sid-progress" {
		t.Fatalf("a later progress frame replaced the earliest session id: %q", w.SessionID)
	}
	// task_started outranks all three; a live one stamps its arrival.
	tr.Observe(started("w1", "real"), t0.Add(time.Minute))
	w = get(t, tr, "w1")
	if w.StartedAt != t0.Add(time.Minute).UnixMilli() || w.SessionID != "sid-launch" || w.Name != "real" ||
		w.Src != (FieldSrc{Name: NameFromLaunch, StartedAt: StartedFromLive, SessionID: SessionFromLaunch}) {
		t.Fatalf("after task_started: %+v src %+v", *w, w.Src)
	}
	// Lower grades no longer override.
	ev = progress("w1", clievent.WorkflowItem{Type: clievent.WorkflowItemAgent, Index: 1, QueuedAt: 1, State: "start"})
	ev.TaskSummary = "fallback2"
	tr.Observe(ev, t0)
	if w := get(t, tr, "w1"); w.StartedAt != t0.Add(time.Minute).UnixMilli() || w.Name != "real" || w.SessionID != "sid-launch" {
		t.Fatalf("lower grade overrode: %+v", *w)
	}
	// An earlier snapshot time still improves a snapshot-graded StartedAt.
	tr.Observe(progress("w2", running(1, "x")), t0)
	tr.Observe(progress("w2", clievent.WorkflowItem{Type: clievent.WorkflowItemAgent, Index: 1, QueuedAt: 2, State: "start"}), t0)
	if w := get(t, tr, "w2"); w.StartedAt != 2 {
		t.Fatalf("snapshot grade kept a later time: %d", w.StartedAt)
	}
}

func TestDegradedDecodeErrors(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	tr.Observe(progress("w1", running(1, "a1")), t0)
	if w := get(t, tr, "w1"); w.Degraded != "" {
		t.Fatalf("clean snapshot degraded %q", w.Degraded)
	}
	partial := progress("w1", running(1, "a1"), running(2, "a2"))
	partial.WorkflowDecode = clievent.WorkflowDecodePartial
	tr.Observe(partial, t0)
	if w := get(t, tr, "w1"); w.Degraded != DegradedDecodeError || len(w.Agents) != 2 {
		t.Fatalf("partial: %q, %d rows", w.Degraded, len(w.Agents))
	}
	tr.Observe(progress("w1", running(1, "a1"), running(2, "a2")), t0)
	if w := get(t, tr, "w1"); w.Degraded != "" {
		t.Fatalf("a clean snapshot did not clear decode_error: %q", w.Degraded)
	}
	identity := itemsIdentityTotal.Value()
	failed := &clievent.Event{Type: "system", SubType: "task_progress", TaskID: "w1", Description: "P: z", WorkflowDecode: clievent.WorkflowDecodeFailed}
	tr.Observe(failed, t0)
	if w := get(t, tr, "w1"); w.Degraded != DegradedDecodeError || len(w.Agents) != 2 || w.Current != "P: z" || w.SnapshotSeq != 3 {
		t.Fatalf("failed snapshot must keep the rows and update the header: %+v", *w)
	}
	if itemsIdentityTotal.Value() <= identity {
		t.Error("identity counter did not move")
	}
	tr.Observe(started("w9", "header only"), t0)
	if w := get(t, tr, "w9"); w.Degraded != DegradedNoSnapshot || w.Agents != nil {
		t.Fatalf("no snapshot yet: %q", w.Degraded)
	}
}

func TestNoteDropped(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	if tr.NoteDropped("w1", t0) {
		t.Fatal("NoteDropped on an unknown task reported true")
	}
	tr.Observe(progress("w1", running(1, "a1")), t0)
	rows := get(t, tr, "w1").Agents
	if !tr.NoteDropped("w1", t0.Add(time.Second)) {
		t.Fatal("NoteDropped on a tracked workflow reported false")
	}
	w := get(t, tr, "w1")
	if w.Degraded != DegradedSnapshotDropped || &w.Agents[0] != &rows[0] || w.LastObservedAt != t0.Add(time.Second).UnixMilli() {
		t.Fatalf("dropped: %+v", *w)
	}
	tr.Observe(header("w1", "x"), t0)
	if get(t, tr, "w1").Degraded != DegradedSnapshotDropped {
		t.Fatal("a frame without a snapshot cleared snapshot_dropped")
	}
	tr.Observe(progress("w1", running(1, "a1")), t0)
	if get(t, tr, "w1").Degraded != "" {
		t.Fatal("the next snapshot did not clear snapshot_dropped")
	}
	tr.Observe(updated("w1", "completed", 1), t0)
	if tr.NoteDropped("w1", t0) {
		t.Fatal("NoteDropped on a terminal workflow reported true")
	}
}

// TestCaps covers the per-process bounds: 16 unsettled workflows with
// tracked, 5 terminal. Not parallel: it reads a process-wide counter.
func TestCaps(t *testing.T) {
	tr := New(nil)
	before := untrackedTotal.Value()
	for i := 1; i <= maxTracked+1; i++ {
		tr.Observe(progress(fmt.Sprintf("w%02d", i), running(1, "a")), t0)
	}
	set := tr.Load()
	if len(set.Workflows) != maxTracked {
		t.Fatalf("%d tracked, want %d", len(set.Workflows), maxTracked)
	}
	tr.Observe(progress(fmt.Sprintf("w%02d", maxTracked+1), running(1, "a")), t0) // counted once
	if d := untrackedTotal.Value() - before; d != 1 {
		t.Errorf("untracked counter moved by %d, want 1", d)
	}
	for i := 1; i <= maxTracked; i++ {
		w := get(t, tr, fmt.Sprintf("w%02d", i))
		if withRows := i <= maxRowWorkflows; withRows != (w.Agents != nil) || (w.Degraded == DegradedTooMany) == withRows {
			t.Fatalf("w%02d: rows %v degraded %q", i, w.Agents != nil, w.Degraded)
		}
		if w.Counts.Total != 1 || w.Status != StatusRunning {
			t.Fatalf("w%02d header-only entry lost its counts or status: %+v", i, *w)
		}
	}
	// A workflow keeps its rows on its next snapshot, all 16 slots taken.
	tr.Observe(progress("w16", running(1, "a"), running(2, "b")), t0)
	if w := get(t, tr, "w16"); len(w.Agents) != 2 || w.Degraded != "" {
		t.Fatalf("w16 lost its rows on its second snapshot: %d rows, %q", len(w.Agents), w.Degraded)
	}
	// A row slot frees up when a workflow with rows ends.
	tr.Observe(updated("w01", "completed", 1), t0)
	tr.Observe(progress("w17", running(1, "a")), t0)
	if w := get(t, tr, "w17"); w.Agents == nil || w.Degraded != "" {
		t.Fatalf("w17 not granted the freed row slot: %q", w.Degraded)
	}
	// Terminal entries: the newest maxTerminal by EndedAt stay.
	for i := 2; i <= 8; i++ {
		tr.Observe(updated(fmt.Sprintf("w%02d", i), "completed", int64(i)), t0)
	}
	var ended []string
	for _, w := range tr.Load().Workflows {
		if IsTerminal(w.Status) {
			ended = append(ended, w.TaskID)
		}
	}
	if fmt.Sprint(ended) != "[w08 w07 w06 w05 w04]" {
		t.Fatalf("terminal entries kept: %v", ended)
	}
	// An evicted id's late frame does not bring back a stub.
	tr.Observe(notification("w02", "completed"), t0)
	for _, w := range tr.Load().Workflows {
		if w.TaskID == "w02" {
			t.Fatal("late frame rebuilt an evicted entry")
		}
	}
}

func TestCaps_RowsAndPhases(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	var items []clievent.WorkflowItem
	for i := maxPhases + 5; i >= 1; i-- { // descending: the Tracker sorts
		items = append(items, phase(i, fmt.Sprint("p", i)))
	}
	for i := maxAgents + 10; i >= 1; i-- {
		items = append(items, running(i, fmt.Sprintf("a%d", i)))
	}
	tr.Observe(progress("w1", items...), t0)
	w := get(t, tr, "w1")
	if len(w.Agents) != maxAgents || !w.AgentsCapped || w.Counts.Total != maxAgents+10 || w.Counts.Running != maxAgents+10 {
		t.Fatalf("rows %d capped %v counts %+v", len(w.Agents), w.AgentsCapped, w.Counts)
	}
	if w.Agents[0].Index != 1 || w.Agents[maxAgents-1].Index != maxAgents {
		t.Fatalf("rows not the lowest indexes ascending: %d..%d", w.Agents[0].Index, w.Agents[maxAgents-1].Index)
	}
	if len(w.Phases) != maxPhases || w.Phases[0].Index != 1 || w.Phases[maxPhases-1].Index != maxPhases || w.Degraded != DegradedPhasesCapped {
		t.Fatalf("phases %d (%d..%d) degraded %q", len(w.Phases), w.Phases[0].Index, w.Phases[len(w.Phases)-1].Index, w.Degraded)
	}
	if w.Phases[0].Counts.Total != maxAgents+10 {
		t.Fatalf("phase counts %+v", w.Phases[0].Counts)
	}
	tr.Observe(updated("w1", "killed", 1), t0)
	if w := get(t, tr, "w1"); w.Counts.Stopped != maxAgents+10 || w.Counts.Running != 0 {
		t.Fatalf("terminal counts past the row cap: %+v", w.Counts)
	}
}

// TestWithheldScript: withholdScriptFromSdkEvents renames phases "phase N"
// and drops the previews; the titles show as they are.
func TestWithheldScript(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	tr.Observe(progress("w1", phase(1, "phase 1"), phase(2, "phase 2"), running(1, "a1")), t0)
	if w := get(t, tr, "w1"); w.Phases[0].Title != "phase 1" || w.Phases[1].Title != "phase 2" || w.Degraded != "" {
		t.Fatalf("withheld: %+v", w.Phases)
	}
}

func TestSourceAndObservedAt(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	tr.Observe(started("w1", "x"), t0)
	w := get(t, tr, "w1")
	if w.Source != SourceStream || w.LastObservedAt != t0.UnixMilli() {
		t.Fatalf("live: %q %d", w.Source, w.LastObservedAt)
	}
	tr.Observe(header("w1", "y"), time.Time{})
	if w := get(t, tr, "w1"); w.LastObservedAt != t0.UnixMilli() {
		t.Fatalf("a frame without a time moved LastObservedAt: %d", w.LastObservedAt)
	}
}

func TestSetOrder(t *testing.T) {
	t.Parallel()
	wfs := []*Workflow{
		{TaskID: "t-old", Status: StatusCompleted, EndedAt: 1},
		{TaskID: "r-nostart", Status: StatusRunning},
		{TaskID: "t-new", Status: StatusFailed, EndedAt: 9},
		{TaskID: "r-late", Status: StatusPaused, StartedAt: 20},
		{TaskID: "u", Status: StatusUnknown, StartedAt: 15},
		{TaskID: "r-early", Status: StatusRunning, StartedAt: 10},
	}
	sortWorkflows(wfs)
	var ids []string
	for _, w := range wfs {
		ids = append(ids, w.TaskID)
	}
	if fmt.Sprint(ids) != "[r-early u r-late r-nostart t-new t-old]" {
		t.Fatalf("order %v", ids)
	}
}

func TestVersionAndCallback(t *testing.T) {
	t.Parallel()
	var calls int
	var tr *Tracker
	tr = New(func() {
		calls++
		tr.KnowTasks([]string{"x"}) // would deadlock if called under the lock
	})
	if v := tr.Load().Version; v != 0 || tr.Load().Workflows != nil {
		t.Fatalf("fresh Set: %+v", tr.Load())
	}
	tr.Observe(started("w1", "a"), t0)
	tr.Observe(&clievent.Event{Type: "system", SubType: "init"}, t0)
	tr.Observe(header("wx", "not a workflow"), t0)
	if v := tr.Load().Version; v != 1 || calls != 1 {
		t.Fatalf("version %d, %d callbacks; want 1, 1", v, calls)
	}
	prev := get(t, tr, "w1")
	tr.Observe(header("w1", "b"), t0)
	if w := get(t, tr, "w1"); w.TrackerVersion != prev.TrackerVersion+1 || tr.Load().Version != 2 || calls != 2 {
		t.Fatalf("second frame: tracker version %d, set %d, %d calls", w.TrackerVersion, tr.Load().Version, calls)
	}
}

// TestConcurrentWriters runs the read loop's Observe, the board's
// ApplyResultFile and KnowTasks and lock-free readers together (-race).
func TestConcurrentWriters(t *testing.T) {
	t.Parallel()
	tr := New(func() {})
	stop := make(chan struct{})
	var writers, reader sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		for i := 0; i < 2000; i++ {
			id := fmt.Sprintf("w%d", i%40)
			tr.Observe(progress(id, running(1, "a"), running(2, fmt.Sprint("b", i))), t0)
			if i%7 == 0 {
				tr.Observe(updated(id, "completed", int64(i)), t0)
			}
		}
	}()
	go func() {
		defer writers.Done()
		for i := 0; i < 2000; i++ {
			tr.ApplyResultFile(&ResultFile{TaskID: fmt.Sprintf("w%d", i%40), Status: "completed", WorkflowProgress: []clievent.WorkflowItem{running(3, "c")}})
			tr.KnowTasks([]string{fmt.Sprint("k", i)})
		}
	}()
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, w := range tr.Load().Workflows {
				_ = w.Counts.Total + len(w.Agents)
				_, _ = json.Marshal(w.Wire(w.Agents))
			}
		}
	}()
	writers.Wait()
	close(stop)
	reader.Wait()
	if n := len(tr.Load().Workflows); n == 0 || n > maxTracked+maxTerminal {
		t.Fatalf("%d entries after the run", n)
	}
}

// TestRememberedIDsBounded: the ids judged as workflows are a FIFO of
// maxRemembered, so a forgotten id's frames no longer build an entry.
func TestRememberedIDsBounded(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	ids := make([]string, maxRemembered+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("k%03d", i)
	}
	tr.KnowTasks(ids)
	if len(tr.ids) != maxRemembered || len(tr.idOrder) != maxRemembered {
		t.Fatalf("%d ids, %d in order; want %d", len(tr.ids), len(tr.idOrder), maxRemembered)
	}
	tr.Observe(header("k000", "x"), t0)
	tr.Observe(header("k001", "x"), t0)
	if wfs := tr.Load().Workflows; len(wfs) != 1 || wfs[0].TaskID != "k001" {
		t.Fatalf("want only the still-remembered k001 built: %+v", wfs)
	}
}

// TestResultFileWithoutSnapshot: an entry the file filled in is not
// no_snapshot when a later frame arrives.
func TestResultFileWithoutSnapshot(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	tr.Observe(started("w1", "wf"), t0)
	if !tr.ApplyResultFile(resultFor("w1", doneItem(1, "a1", "A"))) {
		t.Fatal("not merged")
	}
	tr.Observe(notification("w1", "completed"), t0)
	if w := get(t, tr, "w1"); w.Degraded != "" || len(w.Agents) != 1 {
		t.Fatalf("after the notification: %q, %d rows", w.Degraded, len(w.Agents))
	}
}

// TestEndTimeAfterTerminal: a replayed notification ends the run without a
// time; a later task_updated's end_time fills it in, status or not.
func TestEndTimeAfterTerminal(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"", "completed"} {
		tr := New(nil)
		tr.Observe(started("w1", "wf"), time.Time{})
		tr.Observe(notification("w1", "completed"), time.Time{})
		if w := get(t, tr, "w1"); w.Status != StatusCompleted || w.EndedAt != 0 {
			t.Fatalf("seeded terminal: %s ended %d", w.Status, w.EndedAt)
		}
		tr.Observe(updated("w1", status, 77), t0)
		if w := get(t, tr, "w1"); w.EndedAt != 77 || w.Status != StatusCompleted {
			t.Errorf("patch status %q: %s ended %d, want completed at 77", status, w.Status, w.EndedAt)
		}
	}
}
