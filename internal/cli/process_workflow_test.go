package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const (
	probeTaskID     = "w113pvmto"
	probeResultFile = "workflow/testdata/run/wf_2997921d-435.json"
)

// probeResultFileParsed is the probe run's result file, as the board reads it.
func probeResultFileParsed(t *testing.T) *workflow.ResultFile {
	t.Helper()
	data, err := os.ReadFile(probeResultFile)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := workflow.ParseResultFile(data)
	if err != nil {
		t.Fatal(err)
	}
	return rf
}

// onlyWorkflow returns the Set's single entry.
func onlyWorkflow(t *testing.T, set *workflow.Set) *workflow.Workflow {
	t.Helper()
	if len(set.Workflows) != 1 {
		t.Fatalf("Set holds %d workflows, want 1: %+v", len(set.Workflows), set.Workflows)
	}
	return set.Workflows[0]
}

// drainEvents collects eventCh until the read loop closes it.
func drainEvents(p *Process) []clievent.Event {
	var got []clievent.Event
	for ev := range p.eventCh {
		got = append(got, ev)
	}
	return got
}

// TestProcessWorkflow_LiveProbe feeds the capture through a fake shim with no
// Send in flight (the session is idle while a background workflow runs): the
// Set matches the run's result file, every change wakes the callback once,
// outside the Tracker's lock, and the workflow's ring entries are tagged.
func TestProcessWorkflow_LiveProbe(t *testing.T) {
	t.Parallel()
	p, srv := shimTestPair(&ClaudeProtocol{})
	var wakes atomic.Int64
	p.SetOnWorkflowChange(func() {
		wakes.Add(1)
		p.KnowWorkflowTasks(nil) // takes the Tracker's lock: deadlocks if called under it
		_ = p.Workflows()
	})
	go p.readLoop()
	for _, line := range readWorkflowProbe(t) {
		srv.SendStdout(line)
	}
	srv.SendCLIExited(0)
	events := drainEvents(p)

	set := p.Workflows()
	got := onlyWorkflow(t, set)
	if wakes.Load() != int64(set.Version) || set.Version == 0 {
		t.Errorf("callback woken %d times for Set version %d, want one per change", wakes.Load(), set.Version)
	}
	want, ok := workflow.MergeResultFile(got, probeResultFileParsed(t))
	if !ok {
		t.Fatal("the probe's result file did not merge into the live entry")
	}
	if got.Status != workflow.StatusCompleted || got.Status != want.Status || got.Counts != want.Counts ||
		got.Tokens != want.Tokens || !reflect.DeepEqual(got.Agents, want.Agents) || !reflect.DeepEqual(got.Phases, want.Phases) {
		t.Errorf("live entry disagrees with the result file:\n live %+v\n file %+v", *got, *want)
	}
	if got.Name != "probe" || got.RunID != "wf_2997921d-435" || got.Source != workflow.SourceStream || got.LastObservedAt == 0 {
		t.Errorf("live header = name %q run %q source %q observed %d", got.Name, got.RunID, got.Source, got.LastObservedAt)
	}

	tagged := 0
	for _, e := range p.eventLog.Entries() {
		switch e.Type {
		case clievent.KindTaskStart, clievent.KindTaskProgress, clievent.KindTaskDone:
			if e.TaskType != TaskTypeWorkflow {
				t.Errorf("%s entry of the workflow has TaskType %q", e.Type, e.TaskType)
			}
			tagged++
		}
	}
	if tagged != 9 { // task_started, 6 progress, task_updated, the notification
		t.Errorf("%d task entries in the ring, want 9", tagged)
	}
	for _, ev := range events {
		if strings.HasPrefix(ev.SubType, "task_") && ev.SubType != "task_started" && !ev.WorkflowTask {
			t.Errorf("%s frame left eventCh without WorkflowTask", ev.SubType)
		}
		if ev.WorkflowProgress != nil {
			t.Errorf("%s frame left eventCh with its snapshot", ev.SubType)
		}
	}
}

// TestProcessWorkflow_TaggedEntriesSkipResolve: a workflow whose task id has
// no w-prefix shape is still kept from the linker by InjectHistory once its
// task_start has left the persisted window, because its entries are tagged.
func TestProcessWorkflow_TaggedEntriesSkipResolve(t *testing.T) {
	t.Parallel()
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()
	for _, line := range []string{
		`{"type":"system","subtype":"task_started","task_id":"q7wfabc12","tool_use_id":"toolu_1","description":"probe","task_type":"local_workflow","session_id":"s1"}`,
		`{"type":"system","subtype":"task_progress","task_id":"q7wfabc12","tool_use_id":"toolu_1","description":"Ask: A","summary":"probe","session_id":"s1"}`,
		`{"type":"system","subtype":"task_updated","task_id":"q7wfabc12","patch":{"status":"completed","end_time":1},"session_id":"s1"}`,
		`{"type":"system","subtype":"task_notification","task_id":"q7wfabc12","tool_use_id":"toolu_1","status":"completed","summary":"done","session_id":"s1"}`,
		// An agent task with a summary is no workflow.
		`{"type":"system","subtype":"task_progress","task_id":"a7k2m9p4q","tool_use_id":"toolu_2","description":"Explore","summary":"map the repo","session_id":"s1"}`,
	} {
		srv.SendStdout(line)
	}
	srv.SendCLIExited(0)
	drainEvents(p)

	var history []clievent.EventEntry
	for _, e := range p.eventLog.Entries() {
		if e.Type == clievent.KindTaskStart {
			continue // evicted from the persisted window
		}
		if e.TaskID == "a7k2m9p4q" && e.TaskType != "" {
			t.Errorf("agent task_progress tagged %q", e.TaskType)
		}
		history = append(history, e)
	}
	if n := len(p.Workflows().Workflows); n != 1 {
		t.Errorf("Set holds %d workflows, want 1 (the agent task is none)", n)
	}

	lp := linkerTestProcess(t)
	lp.InjectHistory(history)
	if resolveKicked(lp, "q7wfabc12") {
		t.Error("tagged workflow progress dispatched a Resolve")
	}
	if !resolveKicked(lp, "a7k2m9p4q") {
		t.Error("orphan agent progress did not dispatch a Resolve")
	}
}

// oversizeLine is a task_progress frame past maxScannerBufBytes once framed.
func oversizeLine(head string) string {
	return head + `"description":"` + strings.Repeat("x", maxScannerBufBytes) + `"}`
}

// TestProcessWorkflow_OversizeSnapshot: a snapshot line too long to read is
// skipped without ending the process; its workflow keeps the last rows and
// shows snapshot_dropped until the next snapshot. Every task_progress line
// is counted, whether or not its task is a workflow or its id is readable.
func TestProcessWorkflow_OversizeSnapshot(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()
	defer func() {
		srv.SendCLIExited(0)
		drainEvents(p)
	}()
	probe := readWorkflowProbe(t)
	before := workflowLinesOversize.Value()
	barrier := func(text string) { // a frame the read loop delivers after everything sent so far
		srv.SendStdout(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`)
		for ev := range p.eventCh {
			if ev.Type == "assistant" && ev.Message != nil && len(ev.Message.Content) == 1 && ev.Message.Content[0].Text == text {
				return
			}
		}
		t.Fatal("event channel closed")
	}
	for _, line := range probe[:8] { // through the third snapshot
		srv.SendStdout(line)
	}
	barrier("rows")
	rows := onlyWorkflow(t, p.Workflows()).Agents

	srv.SendStdout(oversizeLine(`{"type":"system","subtype":"task_progress","task_id":"` + probeTaskID + `",`))
	srv.SendStdout(oversizeLine(`{"type":"system","subtype":"task_progress",`))                              // no id
	srv.SendStdout(oversizeLine(`{"type":"system","subtype":"task_progress","task_id":"a7k2m9p4q",`))        // not a workflow
	srv.SendStdout(oversizeLine(`{"type":"assistant","message":{"subtype":"task_progress","task_id":"w1",`)) // not a task frame
	barrier("dropped")
	if !p.Alive() {
		t.Fatal("the process ended on an oversized line")
	}
	w := onlyWorkflow(t, p.Workflows())
	if w.Degraded != workflow.DegradedSnapshotDropped || !reflect.DeepEqual(w.Agents, rows) {
		t.Errorf("after the oversized snapshot: degraded %q, rows %+v; want snapshot_dropped and %+v", w.Degraded, w.Agents, rows)
	}
	if n := workflowLinesOversize.Value() - before; n != 3 {
		t.Errorf("%s moved by %d, want 3", "naozhi_cli_workflow_lines_oversize_total", n)
	}

	srv.SendStdout(probe[10]) // the next snapshot
	barrier("cleared")
	if w := onlyWorkflow(t, p.Workflows()); w.Degraded != "" || len(w.Agents) != 3 {
		t.Errorf("after the next snapshot: degraded %q, %d rows; want neither dropped nor stale", w.Degraded, len(w.Agents))
	}
}

// cutAfter truncates b just past the first sub.
func cutAfter(b []byte, sub string) []byte {
	return b[:bytes.Index(b, []byte(sub))+len(sub)]
}

// TestOversizeProgressTaskID pins the peek at an oversized envelope's head.
func TestOversizeProgressTaskID(t *testing.T) {
	t.Parallel()
	envelope := func(typ, line string) []byte {
		b, _ := json.Marshal(shim.ServerMsg{Type: typ, Seq: 7, Line: line})
		return b
	}
	progress := `{"type":"system","subtype":"task_progress",`
	for name, tc := range map[string]struct {
		head     []byte
		id       string
		progress bool
	}{
		"workflow snapshot": {envelope("stdout", progress+`"task_id":"w113pvmto","tool_use_id":"t"`), "w113pvmto", true},
		"32-byte id":        {envelope("stdout", progress+`"task_id":"`+strings.Repeat("a", 32)+`"`), strings.Repeat("a", 32), true},
		"33-byte id":        {envelope("stdout", progress+`"task_id":"`+strings.Repeat("a", 33)+`"`), "", true},
		"odd id":            {envelope("stdout", progress+`"task_id":"W1-2"`), "", true},
		"empty id":          {envelope("stdout", progress+`"task_id":""`), "", true},
		"id cut off":        {cutAfter(envelope("stdout", progress+`"task_id":"w113pvmto"`), "w113p"), "", true},
		"no id in head":     {envelope("stdout", progress+`"description":"x"`), "", true},
		"id in a string": {envelope("stdout", progress+`"description":"\"task_id\":\"wfake0001\"","task_id":"wreal0001"`),
			"wreal0001", true},
		"other subtype":      {envelope("stdout", `{"type":"system","subtype":"task_updated","task_id":"w113pvmto"`), "", false},
		"keys in tool input": {envelope("stdout", `{"type":"assistant","message":{"content":[{"input":{"type":"system","subtype":"task_progress","task_id":"w1"}}]}}`), "", false},
		"stderr envelope":    {envelope("stderr", progress+`"task_id":"w113pvmto"`), "", false},
		"not an envelope":    {[]byte(progress + `"task_id":"w113pvmto"`), "", false},
	} {
		id, ok := oversizeProgressTaskID(tc.head)
		if id != tc.id || ok != tc.progress {
			t.Errorf("%s: (%q, %v), want (%q, %v)", name, id, ok, tc.id, tc.progress)
		}
	}
}

// TestAttachReconnected_SeedsWorkflowsBeforeReadLoop: the replay builds the
// Set before the read loop starts (the resolver, called just before it, sees
// the seed's decodes), and live frames then carry on from the seed.
func TestAttachReconnected_SeedsWorkflowsBeforeReadLoop(t *testing.T) {
	probe := readWorkflowProbe(t)
	// A wrapped ring holding only the progress frames (lines 6-13).
	backlog := rvReplays(5001, probe[5], probe[6], probe[7], probe[10], probe[11], probe[12])
	f := newReconnectFixture(t, "s1", backlog,
		shim.ServerMsg{Type: "stdout", Seq: 5007, Line: probe[14]},
		shim.ServerMsg{Type: "stdout", Seq: 5008, Line: probe[15]})
	var reads int
	seededFirst := false
	hooks := ReconnectHooks{ResolveUnknown: func(string) bool { seededFirst = reads > 0; return true }}
	p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "k", 0, countingProtocol{&ClaudeProtocol{}, &reads}, 0, 0, hooks)
	if err != nil {
		t.Fatalf("attachReconnected: %v", err)
	}
	defer p.Kill()
	if !seededFirst {
		t.Error("the replay was not seeded before the verdict was settled")
	}
	<-f.liveRead
	f.drainWrites()
	testhelper.Eventually(t, func() bool {
		set := p.Workflows()
		return len(set.Workflows) == 1 && set.Workflows[0].NotifySummary != ""
	}, 3*time.Second, "the live notification never reached the seeded entry")
	set := p.Workflows()
	w := set.Workflows[0]
	if !set.SeedWrapped || w.Status != workflow.StatusCompleted || len(w.Agents) != 3 || w.SnapshotSeq == 0 {
		t.Errorf("seed + live = wrapped %v, %+v; want a completed entry with the seeded rows", set.SeedWrapped, *w)
	}
	for _, a := range w.Agents {
		if a.State != workflow.AgentDone {
			t.Errorf("agent %s = %s, want done", a.Label, a.State)
		}
	}
}

// TestAttachReconnected_KnownWorkflowTasks: with only a workflow's terminal
// frames left in the ring, the session's known ids decide whether the seed
// builds an entry.
func TestAttachReconnected_KnownWorkflowTasks(t *testing.T) {
	probe := readWorkflowProbe(t)
	for name, known := range map[string][]string{"known": {probeTaskID}, "unknown": nil} {
		t.Run(name, func(t *testing.T) {
			f := newReconnectFixture(t, "s1", rvReplays(5001, probe[14], probe[15]))
			hooks := ReconnectHooks{ResolveUnknown: func(string) bool { return true }, KnownWorkflowTasks: known}
			p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "k", 0, &ClaudeProtocol{}, 0, 0, hooks)
			if err != nil {
				t.Fatalf("attachReconnected: %v", err)
			}
			f.drainWrites()
			defer p.Kill()
			set := p.Workflows()
			if known == nil {
				if len(set.Workflows) != 0 {
					t.Errorf("unknown task seeded: %+v", set.Workflows)
				}
				return
			}
			w := onlyWorkflow(t, set)
			if w.TaskID != probeTaskID || w.Status != workflow.StatusCompleted || len(w.Agents) != 0 || w.LastObservedAt != 0 {
				t.Errorf("seeded %+v, want a header-only completed entry with no observation time", *w)
			}
		})
	}
}

// TestProcessWorkflow_ApplyResult: the board's result-file merge reaches the
// Tracker, and only for the file's own task.
func TestProcessWorkflow_ApplyResult(t *testing.T) {
	t.Parallel()
	p, srv := shimTestPair(&ClaudeProtocol{})
	var wakes atomic.Int64
	p.SetOnWorkflowChange(func() { wakes.Add(1) })
	go p.readLoop()
	for _, line := range readWorkflowProbe(t)[:13] { // running, final snapshot seen
		srv.SendStdout(line)
	}
	srv.SendCLIExited(0)
	drainEvents(p)

	rf := probeResultFileParsed(t)
	other := *rf
	other.TaskID = "wother001"
	before := wakes.Load()
	if p.ApplyWorkflowResult(&other) {
		t.Error("a result file of another task merged")
	}
	if !p.ApplyWorkflowResult(rf) {
		t.Fatal("the run's result file did not merge")
	}
	w := onlyWorkflow(t, p.Workflows())
	if w.Status != workflow.StatusCompleted || w.Source != workflow.SourceResultFile || !w.ResultLoaded {
		t.Errorf("after the merge: %+v", *w)
	}
	if wakes.Load() != before+1 {
		t.Errorf("merge woke the callback %d times, want 1", wakes.Load()-before)
	}

	p.SetOnWorkflowChange(nil)
	if p.ApplyWorkflowResult(nil) || !p.ApplyWorkflowResult(rf) || wakes.Load() != before+1 {
		t.Error("a nil file merged, the file stopped merging, or the cleared callback was woken")
	}
}

// TestProcessWorkflow_KnowTasks: ids the board hands in make a workflow of
// a task whose live frames carry no evidence of one.
func TestProcessWorkflow_KnowTasks(t *testing.T) {
	t.Parallel()
	probe := readWorkflowProbe(t)
	for name, known := range map[string][]string{"known": {probeTaskID}, "unknown": nil} {
		t.Run(name, func(t *testing.T) {
			p, srv := shimTestPair(&ClaudeProtocol{})
			p.KnowWorkflowTasks(known)
			go p.readLoop()
			srv.SendStdout(probe[14]) // task_updated
			srv.SendStdout(probe[15]) // task_notification
			srv.SendCLIExited(0)
			drainEvents(p)
			if n := len(p.Workflows().Workflows); n != len(known) {
				t.Errorf("%d workflows from the terminal frames, want %d", n, len(known))
			}
		})
	}
}

// TestProcessWorkflow_FixtureWithoutTracker: a &Process{} literal has no
// Tracker; every workflow path is a no-op on it.
func TestProcessWorkflow_FixtureWithoutTracker(t *testing.T) {
	t.Parallel()
	p := &Process{}
	ev := decodeOne(t, workflowTaskStartedLine)
	p.observeWorkflow(&ev, time.Now())
	p.seedWorkflows(rvReplays(1, workflowTaskStartedLine), 0, &ClaudeProtocol{}, []string{"w1"})
	p.noteOversizeLine([]byte(`{"type":"stdout","seq":1,"line":"{\"type\":\"system\",\"subtype\":\"task_progress\",\"task_id\":\"w1\"`), time.Now())
	p.KnowWorkflowTasks([]string{"w1"})
	p.SetOnWorkflowChange(func() { t.Error("woken without a Tracker") })
	if set := p.Workflows(); set == nil || len(set.Workflows) != 0 || p.ApplyWorkflowResult(probeResultFileParsed(t)) {
		t.Errorf("Workflows() = %+v; want an empty Set and no merge", set)
	}
}

// TestProcessWorkflow_ConcurrentReaders: the read loop's Observe races the
// board's readers and writers (run under -race).
func TestProcessWorkflow_ConcurrentReaders(t *testing.T) {
	t.Parallel()
	p, srv := shimTestPair(&ClaudeProtocol{})
	p.SetOnWorkflowChange(func() { _ = p.Workflows().Version })
	go p.readLoop()
	probe := readWorkflowProbe(t)
	rf := probeResultFileParsed(t)
	other := *rf
	other.TaskID = "wother001"
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, w := range p.Workflows().Workflows {
					_ = len(w.Agents)
				}
				p.KnowWorkflowTasks([]string{"wknown001"})
				p.ApplyWorkflowResult(&other)
			}
		}()
	}
	for _, line := range probe[:5] {
		srv.SendStdout(line)
	}
	for range 50 {
		for _, i := range []int{5, 6, 7, 10, 11, 12} {
			srv.SendStdout(probe[i])
		}
	}
	srv.SendCLIExited(0)
	drainEvents(p)
	close(stop)
	wg.Wait()
	if w := onlyWorkflow(t, p.Workflows()); w.Status != workflow.StatusRunning || len(w.Agents) != 3 {
		t.Errorf("after the race: %+v", *w)
	}
}
