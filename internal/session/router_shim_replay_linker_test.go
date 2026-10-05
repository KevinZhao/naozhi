package session

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// The shim replay walk hands the SubagentLinker only tasks with an agent
// transcript: a Workflow run (CC 2.1.288 probe frame) and local_bash are
// skipped, agents resolve once per task_id.
func TestReplayLinkerTasks_SkipsWorkflowAndBash(t *testing.T) {
	t.Parallel()
	const (
		workflowStarted = `{"type":"system","subtype":"task_started","task_id":"w113pvmto","tool_use_id":"toolu_bdrk_011p2dbLZ9gzoWeQA1qtTGau","description":"tiny probe","task_type":"local_workflow","workflow_name":"probe","session_id":"04a8fc10-6fa5-4b8e-82ba-621974425917"}`
		agentStarted    = `{"type":"system","subtype":"task_started","task_id":"a7k2m9p4q","tool_use_id":"toolu_agent_1","description":"Explore: map the repo","task_type":"local_agent","session_id":"04a8fc10-6fa5-4b8e-82ba-621974425917"}`
		teammateStarted = `{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"toolu_team_1","description":"reviewer: check","task_type":"in_process_teammate"}`
		bashStarted     = `{"type":"system","subtype":"task_started","task_id":"b4n8c2x7z","tool_use_id":"toolu_bash_1","description":"npm test","task_type":"local_bash"}`
		noToolUseID     = `{"type":"system","subtype":"task_started","task_id":"a0000000z","description":"x","task_type":"local_agent"}`
	)
	replays := []shim.ServerMsg{
		{Type: "replay", Line: workflowStarted},
		{Type: "replay", Line: agentStarted},
		{Type: "replay", Line: bashStarted},
		{Type: "replay", Line: agentStarted},
		{Type: "stdout", Line: teammateStarted},
		{Type: "replay", Line: noToolUseID},
		{Type: "replay", Line: "not json"},
		{Type: "replay", Line: teammateStarted},
	}
	var got []string
	for _, ev := range replayLinkerTasks(&cli.ClaudeProtocol{}, replays) {
		got = append(got, ev.TaskID+"/"+ev.ToolUseID)
	}
	if want := []string{"a7k2m9p4q/toolu_agent_1", "t1/toolu_team_1"}; !slices.Equal(got, want) {
		t.Errorf("replayLinkerTasks = %v, want %v", got, want)
	}
}
