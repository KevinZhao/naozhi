package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// TestItemState walks RFC §4.2's normalization table, CC's special items
// included.
func TestItemState(t *testing.T) {
	t.Parallel()
	errText := json.RawMessage(`"boom"`)
	cases := []struct {
		name    string
		it      clievent.WorkflowItem
		want    AgentState
		wantRaw string
	}{
		{"done", clievent.WorkflowItem{State: "done", AgentID: "a1", StartedAt: 1}, AgentDone, ""},
		{"cached done", clievent.WorkflowItem{State: "done", Cached: true}, AgentDone, ""},
		{"skipped flag", clievent.WorkflowItem{State: "error", Skipped: true, AgentID: "a1", StartedAt: 1}, AgentSkipped, ""},
		{"skipped text", clievent.WorkflowItem{State: "error", Error: json.RawMessage(`"skipped by user"`), AgentID: "a1", StartedAt: 1}, AgentSkipped, ""},
		{"classifier block", clievent.WorkflowItem{State: "error", Blocked: true, Error: errText, QueuedAt: 5}, AgentFailed, ""},
		{"queue catch", clievent.WorkflowItem{State: "error", Error: errText, QueuedAt: 5}, AgentFailed, ""},
		{"rate-limit revoke", clievent.WorkflowItem{State: "error", Error: errText, QueuedAt: 5, Tokens: 10}, AgentFailed, ""},
		{"failed", clievent.WorkflowItem{State: "failed", AgentID: "a1", StartedAt: 1}, AgentFailed, ""},
		{"probe line 6 B: start, queuedAt only", clievent.WorkflowItem{State: "start", QueuedAt: 5}, AgentQueued, ""},
		{"rate-limit wait", clievent.WorkflowItem{State: "start", QueuedAt: 5, Tokens: 10, ToolCalls: 2}, AgentQueued, ""},
		{"no state, not started", clievent.WorkflowItem{}, AgentQueued, ""},
		{"start", clievent.WorkflowItem{State: "start", AgentID: "a1", StartedAt: 1}, AgentRunning, ""},
		{"progress with startedAt only", clievent.WorkflowItem{State: "progress", StartedAt: 1}, AgentRunning, ""},
		{"progress with agentId only", clievent.WorkflowItem{State: "progress", AgentID: "a1"}, AgentRunning, ""},
		{"unknown", clievent.WorkflowItem{State: "thinking", AgentID: "a1"}, AgentUnknown, "thinking"},
		{"unknown, long", clievent.WorkflowItem{State: strings.Repeat("x", 40), AgentID: "a1"}, AgentUnknown, strings.Repeat("x", 32) + "..."},
	}
	for _, c := range cases {
		got, raw := itemState(&c.it)
		if got != c.want || raw != c.wantRaw {
			t.Errorf("%s: itemState = %q/%q, want %q/%q", c.name, got, raw, c.want, c.wantRaw)
		}
	}
}

func TestStatusMaps(t *testing.T) {
	t.Parallel()
	type m = func(string) (Status, string)
	cases := []struct {
		name    string
		f       m
		in      string
		want    Status
		wantRaw string
	}{
		{"patch completed", patchStatus, "completed", StatusCompleted, ""},
		{"patch failed", patchStatus, "failed", StatusFailed, ""},
		{"patch killed", patchStatus, "killed", StatusKilled, ""},
		{"patch paused", patchStatus, "paused", StatusPaused, ""},
		{"patch running", patchStatus, "running", StatusRunning, ""},
		{"patch pending", patchStatus, "pending", StatusRunning, ""},
		{"patch other", patchStatus, "adopted", StatusUnknown, "adopted"},
		{"notification completed", notificationStatus, "completed", StatusCompleted, ""},
		{"notification failed", notificationStatus, "failed", StatusFailed, ""},
		{"notification stopped", notificationStatus, "stopped", StatusKilled, ""},
		{"notification other", notificationStatus, "killed", StatusUnknown, "killed"},
		{"file completed", resultFileStatus, "completed", StatusCompleted, ""},
		{"file failed", resultFileStatus, "failed", StatusFailed, ""},
		{"file killed", resultFileStatus, "killed", StatusKilled, ""},
		{"file other", resultFileStatus, strings.Repeat("y", 40), StatusUnknown, strings.Repeat("y", 32) + "..."},
	}
	for _, c := range cases {
		if got, raw := c.f(c.in); got != c.want || raw != c.wantRaw {
			t.Errorf("%s: %q → %q/%q, want %q/%q", c.name, c.in, got, raw, c.want, c.wantRaw)
		}
	}
}

func TestStick(t *testing.T) {
	t.Parallel()
	var am agentMemo
	am.stick("")
	if am.agentID != "" || am.prev != nil {
		t.Fatalf("empty id on a fresh row: %+v", am)
	}
	am.stick("a1")
	am.stick("") // rate-limit requeue drops agentId from the item
	if am.agentID != "a1" || am.prev != nil {
		t.Fatalf("empty id did not keep the last one: %+v", am)
	}
	am.stick("a2") // retry: new attempt, new id
	if am.agentID != "a2" || fmt.Sprint(am.prev) != "[a1]" {
		t.Fatalf("retry: %+v", am)
	}
	published := am.prev
	for i := 3; i <= 12; i++ {
		am.stick(fmt.Sprintf("a%d", i))
	}
	if am.agentID != "a12" || fmt.Sprint(am.prev) != "[a4 a5 a6 a7 a8 a9 a10 a11]" {
		t.Fatalf("history not the newest %d: %+v", maxPrevAgentIDs, am)
	}
	if fmt.Sprint(published) != "[a1]" {
		t.Fatalf("a published PrevAgentIDs slice was mutated: %v", published)
	}
	am.stick("a7") // an earlier id coming back leaves the history
	if am.agentID != "a7" || fmt.Sprint(am.prev) != "[a4 a5 a6 a8 a9 a10 a11 a12]" {
		t.Fatalf("returning id: %+v", am)
	}
}

// TestClipRedactsBeforeTruncating puts a key across each cap: truncating
// first would cut it below RedactSecrets' minimum tail and leak the stub.
func TestClipRedactsBeforeTruncating(t *testing.T) {
	t.Parallel()
	key := "sk-" + strings.Repeat("A1b2C3d4", 6)
	for _, max := range []int{maxLabelRunes, maxLastToolRunes, maxToolSummaryRunes, maxErrorRunes, maxHeaderTextRunes} {
		s := strings.Repeat("x", max-10) + " " + key
		got := clip(s, max)
		if strings.Contains(got, "sk-A1b2") {
			t.Errorf("cap %d: key stub leaked: %q", max, got)
		}
		if n := utf8.RuneCountInString(got); n > max+3 {
			t.Errorf("cap %d: %d runes", max, n)
		}
		if naive := textutil.RedactSecrets(textutil.TruncateRunes(s, max)); !strings.Contains(naive, "sk-A1b2") {
			t.Fatalf("cap %d: the fixture no longer exercises the cut (truncate-then-redact hides it too)", max)
		}
	}
}

// countRedactions swaps the redactor for one that counts. Not parallel-safe.
func countRedactions(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := redactSecrets
	redactSecrets = func(s string) string { n.Add(1); return orig(s) }
	t.Cleanup(func() { redactSecrets = orig })
	return &n
}

func agentItem(idx int, label, summary string) clievent.WorkflowItem {
	return clievent.WorkflowItem{
		Type: clievent.WorkflowItemAgent, Index: idx, Label: label, PhaseIndex: 1,
		AgentID: fmt.Sprintf("a%016x", idx), State: "progress", StartedAt: 1,
		LastToolName: "Bash", LastToolSummary: summary, Error: json.RawMessage(`"e"`),
	}
}

// TestMemoSkipsUnchangedStrings: a snapshot repeating a row's strings costs
// no redaction; changing one string redacts that one only.
func TestMemoSkipsUnchangedStrings(t *testing.T) {
	n := countRedactions(t)
	tr := New(nil)
	items := []clievent.WorkflowItem{
		{Type: clievent.WorkflowItemPhase, Index: 1, Title: "Impl"},
		agentItem(1, "one", "FOO=bar go test"),
		agentItem(2, "two", "ls"),
	}
	snap := func(items []clievent.WorkflowItem) *clievent.Event {
		return &clievent.Event{Type: "system", SubType: "task_progress", TaskID: "w1", WorkflowProgress: items}
	}
	tr.Observe(snap(items), time.Now())
	first := n.Load()
	if first == 0 {
		t.Fatal("first snapshot redacted nothing")
	}
	tr.Observe(snap(append([]clievent.WorkflowItem(nil), items...)), time.Now())
	if d := n.Load() - first; d != 0 {
		t.Fatalf("unchanged snapshot redacted %d strings, want 0", d)
	}
	changed := append([]clievent.WorkflowItem(nil), items...)
	changed[2].LastToolSummary = "cat FOO=baz"
	tr.Observe(snap(changed), time.Now())
	if d := n.Load() - first; d != 1 {
		t.Fatalf("one changed string redacted %d times, want 1", d)
	}
	if got := tr.Load().Workflows[0].Agents[1].LastToolSummary; got != textutil.RedactSecrets("cat FOO=baz") {
		t.Fatalf("changed string not recomputed: %q", got)
	}
}

// TestHeaderStringCaps covers the workflow-level strings' caps.
func TestHeaderStringCaps(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 500)
	tr := New(nil)
	now := time.Now()
	tr.Observe(&clievent.Event{Type: "system", SubType: "task_started", TaskID: "w1", TaskType: clievent.TaskTypeWorkflow, WorkflowName: long}, now)
	tr.Observe(&clievent.Event{Type: "system", SubType: "task_progress", TaskID: "w1", TaskSummary: long, Description: long}, now)
	tr.Observe(&clievent.Event{Type: "system", SubType: "task_updated", TaskID: "w1", Patch: &clievent.TaskPatch{Status: long}}, now)
	w := tr.Load().Workflows[0]
	runes := utf8.RuneCountInString
	if runes(w.Name) != maxLabelRunes+3 || runes(w.Description) != maxHeaderTextRunes+3 ||
		runes(w.Current) != maxHeaderTextRunes+3 || runes(w.RawStatus) != maxRawRunes+3 {
		t.Errorf("caps: name %d description %d current %d raw status %d",
			runes(w.Name), runes(w.Description), runes(w.Current), runes(w.RawStatus))
	}
	tr.Observe(&clievent.Event{Type: "system", SubType: "task_notification", TaskID: "w1", Status: "completed", TaskSummary: long}, now)
	if w := tr.Load().Workflows[0]; runes(w.NotifySummary) != maxHeaderTextRunes+3 {
		t.Errorf("notify summary: %d runes", runes(w.NotifySummary))
	}
}
