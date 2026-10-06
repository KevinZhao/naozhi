package workflow

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestEnumLists: every status and agent state the package produces is in
// AllStatuses / AllAgentStates, the lists the dashboard's display tables
// are held to, and every listed status is either unsettled or terminal.
func TestEnumLists(t *testing.T) {
	t.Parallel()
	statuses, states := AllStatuses(), AllAgentStates()
	var gotStatus []Status
	for _, in := range []string{"completed", "failed", "killed", "paused", "running", "pending", "stopped", "adopted"} {
		st, _ := patchStatus(in)
		gotStatus = append(gotStatus, st)
		st, _ = notificationStatus(in)
		gotStatus = append(gotStatus, st)
		if st, ok := resultFileStatus(in); ok {
			gotStatus = append(gotStatus, st)
		}
	}
	running := &Workflow{Status: StatusRunning}
	gotStatus = append(gotStatus, Interrupted(running, 1).Status, Unclaimed(running).Status)
	for _, st := range gotStatus {
		if !slices.Contains(statuses, string(st)) {
			t.Errorf("status %q is produced but not in AllStatuses %v", st, statuses)
		}
	}
	for _, it := range []clievent.WorkflowItem{
		{State: "done"}, {State: "error", Skipped: true}, {State: "error", QueuedAt: 5}, {State: "failed", AgentID: "a"},
		{State: "start", QueuedAt: 5}, {State: "start", AgentID: "a", StartedAt: 1}, {State: "thinking", AgentID: "a"},
		{State: "stopped", AgentID: "a"},
	} {
		if st, _ := itemState(&it); !slices.Contains(states, string(st)) {
			t.Errorf("agent state %q (from %+v) is not in AllAgentStates %v", st, it, states)
		}
	}
	for _, s := range statuses {
		if IsUnsettled(Status(s)) == IsTerminal(Status(s)) {
			t.Errorf("status %q is unsettled and terminal alike", s)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(statuses)))) != len(statuses) || len(slices.Compact(slices.Sorted(slices.Values(states)))) != len(states) {
		t.Errorf("a list repeats a value: %v %v", statuses, states)
	}
}
