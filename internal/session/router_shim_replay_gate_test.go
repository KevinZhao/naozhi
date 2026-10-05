package session

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/shim"
)

// countingProtocol counts the lines a replay walk hands to ReadEvent.
type countingProtocol struct {
	cli.Protocol
	decoded int
}

func (c *countingProtocol) ReadEvent(line string) ([]clievent.Event, bool, error) {
	c.decoded++
	return c.Protocol.ReadEvent(line)
}

// TestReplayLinkerTasks_DecodesOnlyTaskStartedLines: a long workflow fills
// the replay ring with progress frames and ~1ms snapshots; the linker walk
// decodes only lines that begin like a task_started frame, and still finds
// every agent task, however late in the replay.
func TestReplayLinkerTasks_DecodesOnlyTaskStartedLines(t *testing.T) {
	t.Parallel()
	const (
		agentStarted = `{"type":"system","subtype":"task_started","task_id":"a7k2m9p4q","tool_use_id":"toolu_agent_1","description":"Explore: map the repo","task_type":"local_agent"}`
		lateStarted  = `{"type":"system","subtype":"task_started","task_id":"a0000001z","tool_use_id":"toolu_agent_2","description":"reviewer: check","task_type":"local_agent"}`
		progress     = `{"type":"system","subtype":"task_progress","task_id":"w113pvmto","description":"Ask: A","summary":"tiny probe","workflow_progress":[{"type":"workflow_agent","index":1,"label":"A","state":"start"}]}`
		// A tool_use whose input names task_started is not a task_started frame.
		toolUse = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"mcp__x","input":{"type":"system","subtype":"task_started","task_id":"a9","tool_use_id":"t9"}}]}}`
	)
	replays := []shim.ServerMsg{{Type: "replay", Line: agentStarted}}
	for range 500 {
		replays = append(replays, shim.ServerMsg{Type: "replay", Line: progress})
	}
	replays = append(replays,
		shim.ServerMsg{Type: "replay", Line: toolUse},
		shim.ServerMsg{Type: "replay", Line: agentStarted},
		shim.ServerMsg{Type: "replay", Line: lateStarted},
	)
	proto := &countingProtocol{Protocol: &cli.ClaudeProtocol{}}
	var got []string
	for _, ev := range replayLinkerTasks(proto, replays) {
		got = append(got, ev.TaskID)
	}
	if want := []string{"a7k2m9p4q", "a0000001z"}; !slices.Equal(got, want) {
		t.Errorf("replayLinkerTasks = %v, want %v", got, want)
	}
	if proto.decoded != 3 {
		t.Errorf("decoded %d of %d replay lines, want the 3 task_started ones", proto.decoded, len(replays))
	}
}
