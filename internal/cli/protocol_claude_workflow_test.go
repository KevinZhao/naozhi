package cli

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// workflowProbeFixture is a CC 2.1.288 stream-json capture of a 3-agent
// workflow (phase Ask runs A and B in parallel, phase Sum runs C), with home
// paths rewritten to /home/u and the init frames' tool / MCP / skill lists
// trimmed. docs/rfc/workflow-dashboard.md §1.2.1 walks it line by line.
const workflowProbeFixture = "testdata/workflow-probe-3agent.jsonl"

func readWorkflowProbe(tb testing.TB) []string {
	tb.Helper()
	f, err := os.Open(workflowProbeFixture)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
	if len(lines) != 20 {
		tb.Fatalf("%s: %d lines, want the 20 of the capture", workflowProbeFixture, len(lines))
	}
	return lines
}

// wfView is the part of a decoded Event the workflow fields touch.
type wfView struct {
	Type, SubType, TaskID, TaskType, WorkflowName, Description, TaskSummary, Status string
	Patch                                                                           *clievent.TaskPatch
	Items                                                                           int // -1: WorkflowProgress is nil
	Decode                                                                          clievent.WorkflowDecode
	Launch                                                                          *clievent.WorkflowLaunch
	Done                                                                            bool
}

func viewOf(ev clievent.Event, done bool) wfView {
	n := -1
	if ev.WorkflowProgress != nil {
		n = len(ev.WorkflowProgress)
	}
	return wfView{
		Type: ev.Type, SubType: ev.SubType, TaskID: ev.TaskID, TaskType: ev.TaskType,
		WorkflowName: ev.WorkflowName, Description: ev.Description, TaskSummary: ev.TaskSummary,
		Status: ev.Status, Patch: ev.Patch, Items: n, Decode: ev.WorkflowDecode, Launch: ev.WorkflowLaunch, Done: done,
	}
}

func decodeOne(t *testing.T, line string) (clievent.Event, bool) {
	t.Helper()
	events, done, err := (&ClaudeProtocol{}).ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent: %v\nline: %.200s", err, line)
	}
	if len(events) != 1 {
		t.Fatalf("ReadEvent returned %d events, want 1\nline: %.200s", len(events), line)
	}
	return events[0], done
}

// TestReadEvent_WorkflowProbeGolden maps every line of the captured probe.
func TestReadEvent_WorkflowProbeGolden(t *testing.T) {
	t.Parallel()
	const tid = "w113pvmto"
	sys := func(sub string) wfView { return wfView{Type: "system", SubType: sub, Items: -1} }
	progress := func(desc string, items int) wfView {
		return wfView{Type: "system", SubType: "task_progress", TaskID: tid, Description: desc, TaskSummary: "tiny probe", Items: items}
	}
	want := []wfView{
		1:  sys("init"),
		2:  {Type: "assistant", Items: -1},
		3:  sys("background_tasks_changed"),
		4:  {Type: "system", SubType: "task_started", TaskID: tid, TaskType: "local_workflow", WorkflowName: "probe", Description: "tiny probe", Items: -1},
		5:  {Type: "user", Items: -1, Launch: &clievent.WorkflowLaunch{TaskID: tid, WorkflowName: "probe", RunID: "wf_2997921d-435", Summary: "tiny probe", TranscriptDir: "/home/u/.claude/projects/-private-tmp-nz-wfprobe/04a8fc10-6fa5-4b8e-82ba-621974425917/subagents/workflows/wf_2997921d-435"}},
		6:  progress("Ask: A", 4),
		7:  progress("Ask: B", 4),
		8:  progress("Ask: A", 4),
		9:  {Type: "assistant", Items: -1},
		10: {Type: "assistant", Items: -1},
		11: progress("Sum: C", 5),
		12: progress("Sum: C", -1), // description / usage only
		13: progress("Sum: C", 5),
		14: sys("background_tasks_changed"),
		15: {Type: "system", SubType: "task_updated", TaskID: tid, Patch: &clievent.TaskPatch{Status: "completed", EndTime: 1791170031523}, Items: -1},
		16: {Type: "system", SubType: "task_notification", TaskID: tid, Status: "completed", TaskSummary: `Dynamic workflow "tiny probe" completed`, Items: -1},
		17: sys("init"),
		18: {Type: "assistant", Items: -1},
		19: {Type: "result", SubType: "success", Items: -1, Done: true},
		20: {Type: "result", SubType: "success", Items: -1, Done: true},
	}
	lines := readWorkflowProbe(t)
	events := make([]clievent.Event, len(lines)+1)
	for i, line := range lines {
		ev, done := decodeOne(t, line)
		events[i+1] = ev
		if got := viewOf(ev, done); !reflect.DeepEqual(got, want[i+1]) {
			t.Errorf("line %d:\n got %+v\nwant %+v", i+1, got, want[i+1])
		}
	}

	// Line 6: agent B is queued — state "start" yet no agentId / startedAt.
	b := events[6].WorkflowProgress[3]
	if b.Label != "B" || b.State != "start" || b.AgentID != "" || b.StartedAt != 0 || b.QueuedAt != 1791170018846 {
		t.Errorf("line 6 agent B = %+v, want the queued shape", b)
	}
	if u := events[6].Usage; u == nil || *u != (clievent.TaskUsage{DurationMS: 293}) || events[6].LastToolName != "A" {
		t.Errorf("line 6 usage / last tool = %+v / %q", u, events[6].LastToolName)
	}

	// Line 13: the final snapshot, every declared field.
	agent := func(idx, phase int, label, phaseTitle, id string, started, queued, lastProgress, dur, tokens int64) clievent.WorkflowItem {
		return clievent.WorkflowItem{
			Type: clievent.WorkflowItemAgent, Index: idx, Label: label, PhaseIndex: phase, PhaseTitle: phaseTitle,
			AgentID: id, Model: "claude-opus-5-5[1m]", State: "done", Attempt: 1, StartedAt: started, QueuedAt: queued,
			LastProgressAt: lastProgress, DurationMs: dur, Tokens: tokens,
		}
	}
	final := []clievent.WorkflowItem{
		{Type: clievent.WorkflowItemPhase, Index: 1, Title: "Ask"},
		{Type: clievent.WorkflowItemPhase, Index: 2, Title: "Sum"},
		agent(1, 1, "A", "Ask", "a2093755b9a9ce8c0", 1791170018847, 1791170018846, 1791170021030, 2182, 17739),
		agent(2, 1, "B", "Ask", "a0ba344a06862740f", 1791170018848, 1791170018846, 1791170026698, 5858, 17740),
		agent(3, 2, "C", "Sum", "aa4f131ad102bbc7f", 1791170026701, 1791170026700, 1791170031521, 4820, 17738),
	}
	if got := events[13].WorkflowProgress; !reflect.DeepEqual(got, final) {
		t.Errorf("line 13 snapshot:\n got %+v\nwant %+v", got, final)
	}

	// The ring entries: the Workflow tool_use (input = the whole script) shows
	// no script text, and the notification's summary replaces its subtype.
	if e := EventEntriesFromEventAt(events[2], 1); len(e) != 1 || e[0].Detail != "Workflow" || e[0].Summary != "Workflow" {
		t.Errorf("line 2 entries = %+v, want one tool_use with Detail \"Workflow\"", e)
	}
	if e := EventEntriesFromEventAt(events[15], 1); len(e) != 1 || e[0].Summary != "" || e[0].Status != "completed" {
		t.Errorf("line 15 entries = %+v, want Summary \"\" and Status from the patch", e)
	}
	if e := EventEntriesFromEventAt(events[16], 1); len(e) != 1 || e[0].Summary != `Dynamic workflow "tiny probe" completed` {
		t.Errorf("line 16 entries = %+v, want the notification summary", e)
	}
}

// snapshotFrame wraps items (raw JSON array text, or any value) in a
// task_progress frame; extra is spliced in after the items.
func snapshotFrame(items, extra string) string {
	return `{"type":"system","subtype":"task_progress","task_id":"w1","description":"Ask: A","usage":{"total_tokens":7,"tool_uses":1,"duration_ms":9},"summary":"probe","workflow_progress":` + items + extra + `}`
}

const okItems = `[{"type":"workflow_phase","index":1,"title":"Ask"},{"type":"workflow_agent","index":1,"label":"A","agentId":"a1","state":"done","tokens":5},{"type":"workflow_agent","index":2,"label":"B","agentId":"a2","state":"start"}]`

// TestReadEvent_WorkflowDecodeTolerance covers §4.1.1: which type errors
// keep the frame and how its snapshot is graded.
func TestReadEvent_WorkflowDecodeTolerance(t *testing.T) {
	t.Parallel()
	ok, partial, failed := clievent.WorkflowDecodeOK, clievent.WorkflowDecodePartial, clievent.WorkflowDecodeFailed
	cases := []struct {
		name      string
		line      string
		wantErr   bool
		wantItems int // -1: nil
		want      clievent.WorkflowDecode
	}{
		{"clean", snapshotFrame(okItems, ""), false, 3, ok},
		{"key absent", `{"type":"system","subtype":"task_progress","task_id":"w1","description":"Ask: A","usage":{"total_tokens":7,"tool_uses":1,"duration_ms":9},"summary":"probe"}`, false, -1, ok},
		{"null", snapshotFrame("null", ""), false, -1, ok},
		{"empty array is a snapshot", snapshotFrame("[]", ""), false, 0, ok},
		{"non-identity field", snapshotFrame(`[{"type":"workflow_agent","index":1,"label":"A","tokens":"x"},{"type":"workflow_agent","index":2,"label":"B","tokens":3}]`, ""), false, 2, partial},
		{"error raw of any type", snapshotFrame(`[{"type":"workflow_agent","index":1,"state":"error","error":{"msg":"x"}},{"type":"workflow_agent","index":2,"error":7}]`, ""), false, 2, ok},
		{"object, not array", snapshotFrame(`{"type":"workflow_agent"}`, ""), false, -1, failed},
		{"string, not array", snapshotFrame(`"x"`, ""), false, -1, failed},
		{"items not objects", snapshotFrame(`[1,2]`, ""), false, -1, failed},
		{"index string", snapshotFrame(`[{"type":"workflow_agent","index":"3"}]`, ""), false, -1, failed},
		{"type number", snapshotFrame(`[{"type":5,"index":1}]`, ""), false, -1, failed},
		{"agentId number", snapshotFrame(`[{"type":"workflow_agent","index":1,"agentId":1}]`, ""), false, -1, failed},
		{"phaseIndex string", snapshotFrame(`[{"type":"workflow_agent","index":1,"phaseIndex":"1"}]`, ""), false, -1, failed},
		{"state number", snapshotFrame(`[{"type":"workflow_agent","index":1,"state":2}]`, ""), false, -1, failed},
		// encoding/json reports only the first error: the tokens error hides
		// the index one, so the identity re-check must catch it.
		{"identity error behind a partial one", snapshotFrame(`[{"type":"workflow_agent","index":1,"tokens":"x"},{"type":"workflow_agent","index":"3"}]`, ""), false, -1, failed},
		// The same, with an identity field WorkflowItemsValid cannot see:
		// a zeroed agentId, state or phaseIndex looks legitimate.
		{"agentId error behind a partial one", snapshotFrame(`[{"type":"workflow_agent","index":1,"label":5,"agentId":7}]`, ""), false, -1, failed},
		{"state error behind a partial one", snapshotFrame(`[{"type":"workflow_agent","index":1,"tokens":"x"},{"type":"workflow_agent","index":2,"state":2}]`, ""), false, -1, failed},
		{"phaseIndex error behind a partial one", snapshotFrame(`[{"type":"workflow_agent","index":1,"label":5,"phaseIndex":"1"}]`, ""), false, -1, failed},
		{"null item", snapshotFrame(`[{"type":"workflow_agent","index":1},null]`, ""), false, -1, failed},
		{"duplicate agent index", snapshotFrame(`[{"type":"workflow_agent","index":1},{"type":"workflow_agent","index":1}]`, ""), false, -1, failed},
		// A type error outside the snapshot rejects the frame in either key
		// order; without the second pass the first order would slip through
		// with Status silently zeroed.
		{"hidden error after snapshot", snapshotFrame(`[{"type":"workflow_agent","index":1,"tokens":"x"}]`, `,"status":5`), true, 0, 0},
		{"hidden error before snapshot", `{"type":"system","subtype":"task_progress","status":5,"workflow_progress":[{"type":"workflow_agent","index":1,"tokens":"x"}]}`, true, 0, 0},
		{"hidden error after failed snapshot", snapshotFrame(`[1]`, `,"usage":"x"`), true, 0, 0},
		{"type error elsewhere", `{"type":"system","subtype":"task_progress","usage":{"total_tokens":"x"}}`, true, 0, 0},
		{"syntax error", snapshotFrame(okItems, `,`), true, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events, _, err := (&ClaudeProtocol{}).ReadEvent(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ReadEvent accepted the frame: %+v", events)
				}
				return
			}
			if err != nil || len(events) != 1 {
				t.Fatalf("ReadEvent = %d events, err %v; want the frame kept", len(events), err)
			}
			ev := events[0]
			got := -1
			if ev.WorkflowProgress != nil {
				got = len(ev.WorkflowProgress)
			}
			if got != tc.wantItems || ev.WorkflowDecode != tc.want {
				t.Errorf("items %d decode %v, want items %d decode %v", got, ev.WorkflowDecode, tc.wantItems, tc.want)
			}
			// The rest of the frame survives a tolerated snapshot error.
			if ev.TaskID != "w1" || ev.Description != "Ask: A" || ev.TaskSummary != "probe" ||
				ev.Usage == nil || *ev.Usage != (clievent.TaskUsage{TotalTokens: 7, ToolUses: 1, DurationMS: 9}) {
				t.Errorf("header damaged: %+v", ev)
			}
		})
	}
}

// TestReadEvent_WorkflowPartialKeepsOtherItems: a zeroed non-identity field
// costs only itself.
func TestReadEvent_WorkflowPartialKeepsOtherItems(t *testing.T) {
	t.Parallel()
	ev, _ := decodeOne(t, snapshotFrame(`[{"type":"workflow_agent","index":1,"label":"A","agentId":"a1","tokens":"x","toolCalls":4},{"type":"workflow_agent","index":2,"label":"B","agentId":"a2","tokens":3}]`, `,"last_tool_name":"A"`))
	want := []clievent.WorkflowItem{
		{Type: clievent.WorkflowItemAgent, Index: 1, Label: "A", AgentID: "a1", ToolCalls: 4},
		{Type: clievent.WorkflowItemAgent, Index: 2, Label: "B", AgentID: "a2", Tokens: 3},
	}
	if ev.WorkflowDecode != clievent.WorkflowDecodePartial || !reflect.DeepEqual(ev.WorkflowProgress, want) || ev.LastToolName != "A" {
		t.Errorf("got decode %v items %+v last tool %q", ev.WorkflowDecode, ev.WorkflowProgress, ev.LastToolName)
	}
}

// TestReadEvent_WorkflowLaunch: only an async local_workflow tool_use_result
// becomes a WorkflowLaunch.
func TestReadEvent_WorkflowLaunch(t *testing.T) {
	t.Parallel()
	user := func(tur string) string {
		return `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"x"}]},"tool_use_result":` + tur + `}`
	}
	launch := `{"status":"async_launched","taskId":"w1","taskType":"local_workflow","workflowName":"probe","runId":"wf_1","summary":"tiny","transcriptDir":"/d","scriptPath":"/s/probe.js"}`
	if ev, _ := decodeOne(t, user(launch)); ev.WorkflowLaunch == nil ||
		*ev.WorkflowLaunch != (clievent.WorkflowLaunch{TaskID: "w1", WorkflowName: "probe", RunID: "wf_1", Summary: "tiny", TranscriptDir: "/d"}) {
		t.Errorf("launch = %+v", ev.WorkflowLaunch)
	}
	for name, line := range map[string]string{
		"agent launch":       user(`{"status":"async_launched","taskId":"a1","taskType":"local_agent"}`),
		"completed workflow": user(`{"status":"completed","taskType":"local_workflow","note":"async_launched"}`),
		"string result":      user(`"Workflow \"async_launched\" text"`),
		"Read of this RFC":   `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"{\"status\":\"async_launched\",\"taskType\":\"local_workflow\"}"}]},"tool_use_result":{"type":"text","file":{"content":"{\"status\":\"async_launched\",\"taskType\":\"local_workflow\"}"}}}`,
		"no tool_use_result": `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"\"async_launched\""}]}}`,
		"assistant frame":    `{"type":"assistant","message":{"role":"assistant","content":[]},"tool_use_result":` + launch + `}`,
	} {
		if ev, _ := decodeOne(t, line); ev.WorkflowLaunch != nil {
			t.Errorf("%s: WorkflowLaunch = %+v, want nil", name, ev.WorkflowLaunch)
		}
	}
}

// FuzzReadEventWorkflow: no input panics, and a kept snapshot is always one a
// reader may apply.
func FuzzReadEventWorkflow(f *testing.F) {
	for _, line := range readWorkflowProbe(f) {
		f.Add(line)
	}
	for _, items := range []string{okItems, "[]", "null", "[1,2]", `[{"index":"3"}]`, `[{"type":"workflow_agent","index":1,"tokens":"x"},{"index":"x"}]`} {
		f.Add(snapshotFrame(items, ""))
		f.Add(snapshotFrame(items, `,"status":5`))
	}
	f.Add(`{"type":"user","tool_use_result":{"status":"async_launched","taskType":"local_workflow"}}`)
	f.Fuzz(func(t *testing.T, line string) {
		events, _, err := (&ClaudeProtocol{}).ReadEventInto(line, nil)
		if err != nil {
			return
		}
		for _, ev := range events {
			if ev.WorkflowDecode == clievent.WorkflowDecodeFailed && ev.WorkflowProgress != nil {
				t.Fatalf("Failed decode kept %d items", len(ev.WorkflowProgress))
			}
			if ev.WorkflowProgress != nil && !clievent.WorkflowItemsValid(ev.WorkflowProgress) {
				t.Fatalf("kept an invalid snapshot: %+v", ev.WorkflowProgress)
			}
		}
	})
}

// bigSnapshot synthesises an n-agent task_progress line shaped like the
// largest real one (398 agents, 496KB: ~1.3KB per agent, two thirds of it
// prompt / result previews). skipKey renames the snapshot key, so the line
// costs what it did before WorkflowProgress was declared.
func bigSnapshot(n int, skipKey bool) string {
	key := "workflow_progress"
	if skipKey {
		key = "workflow_progresz"
	}
	var b strings.Builder
	b.WriteString(`{"type":"system","subtype":"task_progress","task_id":"wbig00001","tool_use_id":"toolu_big","description":"Implement: impl:1.0","usage":{"total_tokens":1,"tool_uses":2,"duration_ms":3},"last_tool_name":"impl:1.0","summary":"batch","` + key + `":[`)
	phases := []string{"Implement", "Review", "Fix", "Merge", "Close", "Wrap-up"}
	for i, p := range phases {
		fmt.Fprintf(&b, `{"type":"workflow_phase","index":%d,"title":%q},`, i+1, p)
	}
	prompt := strings.Repeat("You are executing one PR of a batch that fixes triaged review issues. ", 6)
	result := strings.Repeat(`{\"status\":\"ready\",\"pr_number\":3125,\"branch\":\"fix/x\"}`, 7)
	for i := 1; i <= n; i++ {
		errField := ""
		if i%17 == 0 {
			errField = `"error":"agent exited without calling StructuredOutput",`
		}
		fmt.Fprintf(&b, `{"type":"workflow_agent","index":%d,"label":"impl:%d.0","phaseIndex":%d,"phaseTitle":%q,"agentId":"a%016x","model":"claude-opus-5-5[1m]","state":"done","startedAt":1791029368736,"queuedAt":1791029368726,"attempt":1,"lastToolName":"StructuredOutput","lastToolSummary":"ready",%s"promptPreview":%q,"promptFramed":true,"lastProgressAt":1791029804410,"tokens":29976,"toolCalls":12,"durationMs":435671,"resultPreview":"%s"}`,
			i, i, i%6+1, phases[i%6], i, errField, prompt, result)
		if i < n {
			b.WriteByte(',')
		}
	}
	b.WriteString(`],"uuid":"46226038-ae5d-45c3-a33a-45868db9aa27","session_id":"04a8fc10-6fa5-4b8e-82ba-621974425917"}`)
	return b.String()
}

func TestBigSnapshotDecodes(t *testing.T) {
	t.Parallel()
	line := bigSnapshot(398, false)
	ev, _ := decodeOne(t, line)
	if len(ev.WorkflowProgress) != 404 || ev.WorkflowDecode != clievent.WorkflowDecodeOK {
		t.Fatalf("decoded %d items (%v), want 404 OK", len(ev.WorkflowProgress), ev.WorkflowDecode)
	}
	if n := len(line); n < 400<<10 || n > 600<<10 {
		t.Errorf("398-agent line is %d bytes, want the real snapshot's ~496KB", n)
	}
	if ev, _ := decodeOne(t, bigSnapshot(398, true)); ev.WorkflowProgress != nil {
		t.Error("skipKey line still decoded a snapshot")
	}
}

// BenchmarkReadEvent_WorkflowSnapshot398 is the §1.2.3 budget (≤ 1.5ms,
// ≤ 400KB per op); the Skipped variant is the same line as decoded before
// WorkflowProgress existed.
//
//	go test -run '^$' -bench 'WorkflowSnapshot398' -benchmem -count 5 ./internal/cli/
func BenchmarkReadEvent_WorkflowSnapshot398(b *testing.B) {
	benchReadEventLine(b, bigSnapshot(398, false))
}

func BenchmarkReadEvent_WorkflowSnapshot398Skipped(b *testing.B) {
	benchReadEventLine(b, bigSnapshot(398, true))
}

func benchReadEventLine(b *testing.B, line string) {
	p := &ClaudeProtocol{}
	var buf [2]clievent.Event
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := p.ReadEventInto(line, buf[:0]); err != nil {
			b.Fatal(err)
		}
	}
}

// TestReadEvent_CapturedHookControlFrames pins the line-start prefixes the
// fast path keys on against frames CC 2.1.288 actually wrote (a SessionStart
// hook and a set_model ack, captured under a throwaway HOME). Breaking each
// line's last byte proves the skip / targeted parse ran instead of the full
// unmarshal, which would reject the broken line.
func TestReadEvent_CapturedHookControlFrames(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook-control-2.1.288.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("fixture has %d lines, want hook_started, hook_response, control_response", len(lines))
	}
	p := &ClaudeProtocol{}
	for _, line := range lines[:2] {
		broken := line[:len(line)-1] + "!"
		if events, _, err := p.ReadEvent(broken); err != nil || events != nil {
			t.Errorf("hook frame missed the fast path: events %v err %v\nline: %.120s", events, err, line)
		}
	}
	events, _, err := p.ReadEvent(lines[2])
	if err != nil || len(events) != 1 || events[0].Type != "control_ack" || events[0].RPCRequestID != "req_1" || events[0].SubType != "success" {
		t.Errorf("control_response = %+v, err %v; want a control_ack for req_1", events, err)
	}
	if events, _, err := p.ReadEvent(lines[2][:len(lines[2])-1] + "!"); err != nil || events != nil {
		t.Errorf("control_response missed the fast path: events %v err %v", events, err)
	}
}
