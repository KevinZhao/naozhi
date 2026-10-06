package workflow

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestIsWorkflowTask walks RFC §5.7's rules.
func TestIsWorkflowTask(t *testing.T) {
	t.Parallel()
	mcp := &clievent.Event{Type: "system", SubType: "task_progress", TaskID: "a1b2c3d4e5", TaskSummary: "polling", Description: "mcp task"}
	agentSnap := progress("a9", running(1, "x"))
	agentSnap.SubagentType = "general-purpose"
	agentStart := &clievent.Event{Type: "system", SubType: "task_started", TaskID: "a8", TaskType: "local_agent"}
	bgChanged := &clievent.Event{Type: "system", SubType: "background_tasks_changed"}

	tr := New(nil)
	for _, ev := range []*clievent.Event{mcp, agentSnap, agentStart, bgChanged} {
		if tr.IsWorkflowTask(ev) {
			t.Errorf("%s %s judged a workflow", ev.SubType, ev.TaskID)
		}
		tr.Observe(ev, t0)
		if ev.WorkflowTask {
			t.Errorf("%s %s: Observe set WorkflowTask", ev.SubType, ev.TaskID)
		}
	}
	if n := len(tr.Load().Workflows); n != 0 {
		t.Fatalf("non-workflow frames built %d entries", n)
	}

	// Rule 2: a local_workflow task_started. Rule 2b then holds for the
	// frames that carry no evidence of their own.
	st := started("w1", "wf")
	if !tr.IsWorkflowTask(st) {
		t.Fatal("local_workflow task_started not a workflow")
	}
	if len(tr.Load().Workflows) != 0 {
		t.Fatal("IsWorkflowTask built an entry")
	}
	tr.Observe(st, t0)
	for _, ev := range []*clievent.Event{header("w1", "P: a"), updated("w1", "completed", 1), notification("w1", "completed")} {
		tr.Observe(ev, t0)
		if !ev.WorkflowTask {
			t.Errorf("%s of a tracked workflow: WorkflowTask false", ev.SubType)
		}
	}

	// Rule 3: a snapshot key, even one that failed to decode.
	failed := &clievent.Event{Type: "system", SubType: "task_progress", TaskID: "w2", WorkflowDecode: clievent.WorkflowDecodeFailed}
	tr.Observe(failed, t0)
	if !failed.WorkflowTask {
		t.Error("failed snapshot key not a workflow")
	}
	empty := progress("w3")
	tr.Observe(empty, t0)
	if !empty.WorkflowTask || get(t, tr, "w3").Agents == nil {
		t.Error("an empty snapshot array is a snapshot")
	}

	// Rule 4: known ids, from the board or a launch receipt.
	tr.KnowTasks([]string{"w4"})
	known := updated("w4", "completed", 5)
	tr.Observe(known, t0)
	if !known.WorkflowTask || get(t, tr, "w4").Status != StatusCompleted {
		t.Error("a known task's task_updated did not build a header-only entry")
	}
	launch := &clievent.Event{Type: "user", SessionID: "sid", WorkflowLaunch: &clievent.WorkflowLaunch{TaskID: "w5", WorkflowName: "n", RunID: "wf_1", TranscriptDir: "/d"}}
	tr.Observe(launch, t0)
	if launch.WorkflowTask {
		t.Error("the launch frame is not a task frame; WorkflowTask must stay false")
	}
	if w := get(t, tr, "w5"); w.RunID != "wf_1" || w.LaunchTranscriptDir != "/d" || w.Name != "n" || w.SessionID != "sid" {
		t.Errorf("launch entry: %+v", *w)
	}
	later := header("w5", "P: b")
	tr.Observe(later, t0)
	if !later.WorkflowTask {
		t.Error("a launched task's description-only frame not a workflow")
	}

	// Rule 1 outranks everything: subagent_type on a known id is no workflow.
	vetoed := header("w5", "P: c")
	vetoed.SubagentType = "x"
	if tr.IsWorkflowTask(vetoed) {
		t.Error("subagent_type did not veto")
	}
}
