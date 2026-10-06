package cli

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
	"github.com/naozhi/naozhi/internal/subagent"
)

// workflowTaskStartedLine is the task_started frame CC 2.1.288 emits for a
// Workflow tool run (probe capture; prompt shortened).
const workflowTaskStartedLine = `{"type":"system","subtype":"task_started","task_id":"w113pvmto","tool_use_id":"toolu_bdrk_011p2dbLZ9gzoWeQA1qtTGau","description":"tiny probe","task_type":"local_workflow","workflow_name":"probe","prompt":"export const meta = { name: 'probe', des","uuid":"e5418ca7-93e5-40ec-9490-2099a024a4ec","session_id":"04a8fc10-6fa5-4b8e-82ba-621974425917"}`

const agentTaskStartedLine = `{"type":"system","subtype":"task_started","task_id":"a7k2m9p4q","tool_use_id":"toolu_agent_1","description":"Explore: map the repo","task_type":"local_agent","session_id":"04a8fc10-6fa5-4b8e-82ba-621974425917"}`

// linkerTestProcess returns a Process whose linker never finishes a Resolve:
// the context is set, the transcript is missing and the retry sleep is an
// hour, so a dispatched Resolve keeps its in-flight claim until cleanup.
// That makes resolveKicked a synchronous count of dispatches.
func linkerTestProcess(t *testing.T) *Process {
	t.Helper()
	l := subagent.NewLinker()
	l.ConfigureForTest(int64(time.Hour), 1, 0)
	l.SetContext(t.TempDir(), "04a8fc10-6fa5-4b8e-82ba-621974425917")
	p := &Process{eventLog: ring.NewEventLog(0), done: make(chan struct{}), linker: l}
	l.SetPoolContext(p.lifecycleContext())
	t.Cleanup(func() { close(p.done) })
	return p
}

// resolveKicked reports whether a Resolve was dispatched for taskID. It takes
// the claim itself when none was, so probe each id once.
func resolveKicked(p *Process, taskID string) bool {
	return !p.linker.TryMarkResolveInflight(taskID)
}

func decodeOne(t *testing.T, line string) clievent.Event {
	t.Helper()
	events, _, err := (&ClaudeProtocol{}).ReadEvent(line)
	if err != nil || len(events) != 1 {
		t.Fatalf("ReadEvent = %d events, err %v", len(events), err)
	}
	return events[0]
}

func TestNotifyLinker_SkipsWorkflowTask(t *testing.T) {
	t.Parallel()
	p := linkerTestProcess(t)
	wf := decodeOne(t, workflowTaskStartedLine)
	if wf.TaskType != TaskTypeWorkflow {
		t.Fatalf("decoded task_type = %q, want %q", wf.TaskType, TaskTypeWorkflow)
	}
	p.notifyLinker(wf, time.Now().UnixMilli(), false)
	p.notifyLinker(decodeOne(t, agentTaskStartedLine), time.Now().UnixMilli(), false)

	if resolveKicked(p, "w113pvmto") {
		t.Error("local_workflow task_started dispatched a Resolve")
	}
	if !resolveKicked(p, "a7k2m9p4q") {
		t.Error("local_agent task_started did not dispatch a Resolve")
	}
}

func TestLinkerSkipsTaskType(t *testing.T) {
	t.Parallel()
	for taskType, want := range map[string]bool{
		"local_bash": true, "local_workflow": true,
		"local_agent": false, "in_process_teammate": false, "": false,
	} {
		if got := LinkerSkipsTaskType(taskType); got != want {
			t.Errorf("LinkerSkipsTaskType(%q) = %v, want %v", taskType, got, want)
		}
	}
}

func TestInjectHistory_WorkflowTasksNotResolved(t *testing.T) {
	t.Parallel()
	p := linkerTestProcess(t)
	entries := []clievent.EventEntry{
		// Workflow whose task_start is in the batch: neither row resolves.
		{Type: clievent.KindTaskStart, TaskID: "w113pvmto", ToolUseID: "toolu_wf", TaskType: TaskTypeWorkflow, Time: 1},
		{Type: clievent.KindTaskProgress, TaskID: "w113pvmto", ToolUseID: "toolu_wf", Time: 2},
		// The same task_start replayed without TaskType keeps the verdict.
		{Type: clievent.KindTaskStart, TaskID: "w113pvmto", ToolUseID: "toolu_wf", Time: 2},
		// Progress row tagged as a workflow, task_start evicted.
		{Type: clievent.KindTaskProgress, TaskID: "tagged-wf", TaskType: TaskTypeWorkflow, Time: 3},
		// A task_start without TaskType outranks the id shape.
		{Type: clievent.KindTaskStart, TaskID: "wabcdefgh", ToolUseID: "toolu_w2", Time: 4},
		{Type: clievent.KindTaskProgress, TaskID: "wabcdefgh", ToolUseID: "toolu_w2", Time: 5},
		// Agent task in the batch still resolves.
		{Type: clievent.KindTaskStart, TaskID: "a1b2c3d4e", ToolUseID: "toolu_a", TaskType: "local_agent", Time: 6},
		// Orphan agent progress still resolves.
		{Type: clievent.KindTaskProgress, TaskID: "a9z8y7x6w", Time: 7},
		// local_bash task_start keeps today's behaviour (resolves, tombstones).
		{Type: clievent.KindTaskStart, TaskID: "b00000001", ToolUseID: "toolu_b", TaskType: "local_bash", Time: 8},
	}
	// Legacy history: > 500 orphan progress rows of a workflow whose task_start
	// left the persisted window, no TaskType on any of them.
	for i := range 600 {
		entries = append(entries, clievent.EventEntry{Type: clievent.KindTaskProgress, TaskID: "wq7r3t9k2", Time: int64(100 + i)})
	}
	p.InjectHistory(entries)

	for id, want := range map[string]bool{
		"w113pvmto": false, "tagged-wf": false, "wq7r3t9k2": false,
		"wabcdefgh": true, "a1b2c3d4e": true, "a9z8y7x6w": true, "b00000001": true,
	} {
		if got := resolveKicked(p, id); got != want {
			t.Errorf("task %s: Resolve dispatched = %v, want %v", id, got, want)
		}
	}
	if _, ok := p.linker.Query("wq7r3t9k2"); ok {
		t.Error("legacy workflow orphan left a linker entry (tombstone)")
	}
}

func TestIsWorkflowTaskIDShape(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{
		"w113pvmto": true, "w00000000": true, "wzzzzzzzz": true,
		"a113pvmto": false, "W113pvmto": false, "w113pvmt": false, "w113pvmtoo": false,
		"w113pvMto": false, "w113pvm-o": false, "": false,
	} {
		if got := isWorkflowTaskIDShape(id); got != want {
			t.Errorf("isWorkflowTaskIDShape(%q) = %v, want %v", id, got, want)
		}
	}
}
