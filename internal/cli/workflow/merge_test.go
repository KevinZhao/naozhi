package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

func doneItem(idx int, id, label string) clievent.WorkflowItem {
	return clievent.WorkflowItem{Type: clievent.WorkflowItemAgent, Index: idx, PhaseIndex: 1, AgentID: id, Label: label,
		Model: "m", State: "done", StartedAt: 3, Tokens: int64(100 * idx)}
}

func resultFor(task string, items ...clievent.WorkflowItem) *ResultFile {
	return &ResultFile{TaskID: task, Status: "completed", StartTime: 1000, DurationMs: 500, TotalTokens: 777, TotalToolCalls: 9,
		WorkflowProgress: append([]clievent.WorkflowItem{phase(1, "Ask")}, items...)}
}

// TestApplyResultFile_TaskIDMustMatch: a resumed run shares its runId with
// the earlier attempt, whose result file names the earlier taskId.
func TestApplyResultFile_TaskIDMustMatch(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	tr.Observe(progress("wnew00001", running(1, "a1")), t0)
	if tr.ApplyResultFile(resultFor("wold00001", doneItem(1, "a0", "A"))) {
		t.Fatal("result file of another attempt merged")
	}
	if w := get(t, tr, "wnew00001"); w.Status != StatusRunning || w.ResultLoaded || w.Agents[0].State != AgentRunning {
		t.Fatalf("mismatched file changed the entry: %+v", *w)
	}
	if tr.ApplyResultFile(nil) || tr.ApplyResultFile(&ResultFile{}) {
		t.Fatal("nil / empty result file merged")
	}
}

func TestApplyResultFile_Authoritative(t *testing.T) {
	t.Parallel()
	tr := New(nil)
	tr.Observe(started("w1", "wf"), t0.Add(5*time.Hour)) // StartedAt from a live read, hours after the run began
	other := running(2, "b1")
	tr.Observe(progress("w1", running(1, "a1"), other), t0.Add(5*time.Hour))
	before := get(t, tr, "w1")
	noID := doneItem(1, "", "A") // a file row without agentId keeps the sticky one
	if !tr.ApplyResultFile(resultFor("w1", noID)) {
		t.Fatal("matching result file not merged")
	}
	w := get(t, tr, "w1")
	if w.Status != StatusCompleted || w.StartedAt != 1000 || w.Src.StartedAt != StartedFromResultFile || w.EndedAt != 1500 ||
		w.Tokens != 777 || w.ToolCalls != 9 || w.DurationMS != 500 || w.Source != SourceResultFile || !w.ResultLoaded {
		t.Fatalf("merged header: %+v", *w)
	}
	if len(w.Agents) != 2 || w.Agents[0].AgentID != "a1" || w.Agents[0].Label != "A" || w.Agents[0].Model != "m" ||
		w.Agents[0].Tokens != 100 || w.Agents[0].State != AgentDone || w.Agents[1].State != AgentStopped {
		t.Fatalf("merged rows: %+v", w.Agents)
	}
	if w.Counts != (Counts{Total: 2, Done: 1, Stopped: 1}) || w.Phases[0].Counts != w.Counts || w.Degraded != "" {
		t.Fatalf("merged counts %+v phases %+v degraded %q", w.Counts, w.Phases, w.Degraded)
	}
	if before.Agents[0].State != AgentRunning {
		t.Fatal("merge mutated the previously published rows")
	}
	// Later frames: a notification adds its summary only; a task_updated
	// cannot move the status the file set.
	observeAll(tr, t0, notification("w1", "failed"), updated("w1", "killed", 1))
	if w := get(t, tr, "w1"); w.Tokens != 777 || w.NotifySummary != "note" || w.Status != StatusCompleted || w.EndedAt != 1500 {
		t.Fatalf("after the file: %+v", *w)
	}
}

func TestMergeResultFile_Pure(t *testing.T) {
	t.Parallel()
	w := &Workflow{TaskID: "w1", Status: StatusRunning, Src: FieldSrc{StartedAt: StartedFromRef}, StartedAt: 2000,
		Agents: []Agent{{Index: 1, AgentID: "a2", PrevAgentIDs: []string{"a1"}, State: AgentRunning}, {Index: 4, State: AgentQueued}},
		Phases: []Phase{{Index: 1, Title: "Ask"}}, Degraded: DegradedSnapshotStale}
	if got, ok := MergeResultFile(w, resultFor("w2")); ok || got != w {
		t.Fatal("other task's file merged")
	}
	retry := doneItem(1, "a3", "A")
	got, ok := MergeResultFile(w, resultFor("w1", retry, doneItem(2, "b", "B")))
	if !ok {
		t.Fatal("not merged")
	}
	if fmt.Sprint(got.Agents[0].PrevAgentIDs) != "[a1 a2]" || got.Agents[0].AgentID != "a3" {
		t.Errorf("attempt history lost: %+v", got.Agents[0])
	}
	if len(got.Agents) != 3 || got.Agents[1].Index != 2 || got.Agents[2].Index != 4 || got.Agents[2].State != AgentStopped {
		t.Errorf("rows not merged by index: %+v", got.Agents)
	}
	if got.StartedAt != 1000 || got.Degraded != "" || got.Source != SourceResultFile {
		t.Errorf("header: %+v", *got)
	}
	if w.Agents[0].AgentID != "a2" || w.Degraded != DegradedSnapshotStale || w.Status != StatusRunning {
		t.Error("MergeResultFile mutated its input")
	}
	// A file without items keeps the rows, takes phases from phases[].
	bare := &ResultFile{TaskID: "w1", Status: "killed", Phases: []resultPhase{{"One"}, {"Two"}}}
	got, _ = MergeResultFile(&Workflow{TaskID: "w1", Status: StatusRunning, Agents: []Agent{{Index: 1, PhaseIndex: 2, State: AgentRunning}}}, bare)
	if got.Status != StatusKilled || len(got.Phases) != 2 || got.Phases[1].Title != "Two" || got.Phases[1].Counts.Stopped != 1 || got.Agents[0].State != AgentStopped {
		t.Errorf("bare file: %+v", *got)
	}
}

func TestParseResultFile(t *testing.T) {
	t.Parallel()
	good := `{"taskId":"w1","status":"completed","startTime":5,"result":{"a":1},"logs":["x"],"totalTokens":3,"totalToolCalls":1,"durationMs":2,` +
		`"phases":[{"title":"P"}],"script":"secret script","args":{"k":"v"},` +
		`"workflowProgress":[{"type":"workflow_agent","index":1,"label":"A","model":"m","tokens":7,"promptPreview":"p","resultPreview":"r"}]}`
	rf, err := ParseResultFile([]byte(good))
	if err != nil || rf.TaskID != "w1" || rf.TotalTokens != 3 || len(rf.WorkflowProgress) != 1 || rf.WorkflowProgress[0].Tokens != 7 {
		t.Fatalf("good file: %+v, %v", rf, err)
	}
	// A type error inside the items drops them, not the file.
	rf, err = ParseResultFile([]byte(strings.Replace(good, `"tokens":7`, `"tokens":"7"`, 1)))
	if err != nil || rf.WorkflowProgress != nil || rf.Status != "completed" || rf.TotalTokens != 3 {
		t.Fatalf("item type error: %+v, %v", rf, err)
	}
	// ... unless it hides a type error elsewhere, which fails the file.
	masked := strings.Replace(strings.Replace(good, `"tokens":7`, `"tokens":"7"`, 1), `"status":"completed",`, ``, 1)
	masked = strings.Replace(masked, `"promptPreview":"p","resultPreview":"r"}]`, `"promptPreview":"p","resultPreview":"r"}],"status":5`, 1)
	if _, err := ParseResultFile([]byte(masked)); err == nil {
		t.Fatal("type error behind an item type error accepted")
	}
	if _, err := ParseResultFile([]byte(`{"taskId":5}`)); err == nil {
		t.Fatal("type error outside the items accepted")
	}
	dup := strings.Replace(good, `"workflowProgress":[`, `"workflowProgress":[{"type":"workflow_agent","index":1},`, 1)
	if rf, err := ParseResultFile([]byte(dup)); err != nil || rf.WorkflowProgress != nil {
		t.Fatalf("duplicate index kept: %+v, %v", rf, err)
	}
}

func TestNewResultCache(t *testing.T) {
	t.Parallel()
	key := "ghp_" + strings.Repeat("Ab12", 10)
	c := NewResultCache(&ResultFile{Result: json.RawMessage(`{ "r": ["4", "Paris"], "token": "` + key + `" }`), Logs: []string{"got " + key}})
	if !strings.HasPrefix(c.Result, `{"r":["4","Paris"],"token":"`) {
		t.Fatalf("result not compacted: %q", c.Result)
	}
	if strings.Contains(c.Result, "ghp_Ab12") || strings.Contains(c.Logs[0], "ghp_Ab12") || c.ResultTruncated || c.LogsTruncated {
		t.Fatalf("redaction: %+v", c)
	}
	if NewResultCache(&ResultFile{Result: json.RawMessage(`null`)}).Result != "" {
		t.Fatal("null result not empty")
	}
	big := NewResultCache(&ResultFile{Result: json.RawMessage(`"` + strings.Repeat("é", maxResultBytes) + `"`)})
	if !big.ResultTruncated || len(big.Result) > maxResultBytes || !utf8.ValidString(big.Result) {
		t.Fatalf("big result: %d bytes truncated=%v", len(big.Result), big.ResultTruncated)
	}
	var logs []string
	for i := 0; i < 300; i++ {
		logs = append(logs, fmt.Sprint("line ", i))
	}
	c = NewResultCache(&ResultFile{Logs: logs})
	if len(c.Logs) != maxLogLines || c.Logs[0] != "line 100" || c.Logs[maxLogLines-1] != "line 299" || !c.LogsTruncated {
		t.Fatalf("line cap: %d lines, %q..%q", len(c.Logs), c.Logs[0], c.Logs[len(c.Logs)-1])
	}
	long := strings.Repeat("x", 2000)
	c = NewResultCache(&ResultFile{Logs: []string{long, long, long}})
	if len(c.Logs) != 3 || utf8.RuneCountInString(c.Logs[0]) != maxLogLineRunes+3 {
		t.Fatalf("line rune cap: %d", utf8.RuneCountInString(c.Logs[0]))
	}
	logs = logs[:0]
	for i := 0; i < 150; i++ {
		logs = append(logs, fmt.Sprintf("%03d%s", i, strings.Repeat("y", 497)))
	}
	c = NewResultCache(&ResultFile{Logs: logs})
	size := 0
	for _, l := range c.Logs {
		size += len(l)
	}
	if size > maxLogBytes || !c.LogsTruncated || !strings.HasPrefix(c.Logs[len(c.Logs)-1], "149") || len(c.Logs) != maxLogBytes/500 {
		t.Fatalf("byte cap: %d lines, %d bytes", len(c.Logs), size)
	}
}

func TestMergeRows(t *testing.T) {
	t.Parallel()
	rows := func(idx ...int) []Agent {
		var out []Agent
		for _, i := range idx {
			out = append(out, Agent{Index: i, Label: fmt.Sprint("v", i)})
		}
		return out
	}
	upd := rows(2, 5)
	upd[0].Label = "new"
	got := mergeRows(rows(1, 2, 3, 7), upd)
	want := []Agent{{Index: 1, Label: "v1"}, {Index: 2, Label: "new"}, {Index: 3, Label: "v3"}, {Index: 5, Label: "v5"}, {Index: 7, Label: "v7"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeRows = %+v", got)
	}
}
