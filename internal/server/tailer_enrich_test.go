package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/ring"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// registryWith returns a registry holding one tailer for (key, taskID) whose
// aggregator reports meta.
func registryWith(key, taskID string, meta node.AgentMetaPatch) *tailerRegistry {
	r := newTailerRegistry("")
	r.byTask[tailerKey{key, taskID}] = &agentTailer{meta: meta}
	return r
}

func TestTailerEnrich_OverlaysLaterValues(t *testing.T) {
	t.Parallel()
	r := registryWith("k", "t1", node.AgentMetaPatch{LastTool: "Bash", LastDetail: "ls", ToolUses: 7, DurationMS: 900})
	r.byTask[tailerKey{"k", ""}] = &agentTailer{meta: node.AgentMetaPatch{LastTool: "Write"}}
	snap := &session.SessionSnapshot{Key: "k", Subagents: []ring.SubagentInfo{
		{TaskID: "t1", LastTool: "Read", LastDetail: "a.go", ToolUses: 3, DurationMS: 100},
		{TaskID: "t2", LastTool: "Grep", ToolUses: 4},
		{LastTool: "Edit"},
	}}
	r.enrich(snap)
	got := snap.Subagents[0]
	if got.LastTool != "Bash" || got.LastDetail != "ls" || got.ToolUses != 7 || got.DurationMS != 900 {
		t.Errorf("t1 = %+v, want the tailer's later values", got)
	}
	if s := snap.Subagents[1]; s.LastTool != "Grep" || s.ToolUses != 4 {
		t.Errorf("t2 has no tailer but changed: %+v", s)
	}
	if s := snap.Subagents[2]; s.LastTool != "Edit" {
		t.Errorf("an entry without a task id changed: %+v", s)
	}
}

// The tailer only raises counters and only fills non-empty strings: the
// EventLog's task_notification usage wins when it is ahead.
func TestTailerEnrich_KeepsSnapshotWhenAhead(t *testing.T) {
	t.Parallel()
	r := registryWith("k", "t1", node.AgentMetaPatch{ToolUses: 2, DurationMS: 50})
	snap := &session.SessionSnapshot{Key: "k", Subagents: []ring.SubagentInfo{
		{TaskID: "t1", LastTool: "Read", LastDetail: "a.go", ToolUses: 3, DurationMS: 100},
	}}
	r.enrich(snap)
	if got := snap.Subagents[0]; got.LastTool != "Read" || got.LastDetail != "a.go" || got.ToolUses != 3 || got.DurationMS != 100 {
		t.Errorf("got %+v, want the snapshot's values kept", got)
	}
}

// A tailer registered under another session key does not leak into this one.
func TestTailerEnrich_KeyedBySession(t *testing.T) {
	t.Parallel()
	r := registryWith("other", "t1", node.AgentMetaPatch{LastTool: "Bash"})
	snap := &session.SessionSnapshot{Key: "k", Subagents: []ring.SubagentInfo{{TaskID: "t1"}}}
	r.enrich(snap)
	if snap.Subagents[0].LastTool != "" {
		t.Errorf("another session's tailer was applied: %+v", snap.Subagents[0])
	}
}

func TestTailerEnrich_NilSafe(t *testing.T) {
	t.Parallel()
	snap := &session.SessionSnapshot{Key: "k", Subagents: []ring.SubagentInfo{{TaskID: "t1"}}}
	var r *tailerRegistry
	r.enrich(snap)
	(&Hub{}).enrichSnapshot(snap)
	var h *Hub
	h.enrichSnapshot(snap)
	registryWith("k", "t1", node.AgentMetaPatch{}).enrich(nil)
}

func TestHubEnrichSnapshot_Delegates(t *testing.T) {
	t.Parallel()
	h := &Hub{tailers: registryWith("k", "t1", node.AgentMetaPatch{LastTool: "Bash"})}
	snap := &session.SessionSnapshot{Key: "k", Subagents: []ring.SubagentInfo{{TaskID: "t1"}}}
	h.enrichSnapshot(snap)
	if snap.Subagents[0].LastTool != "Bash" {
		t.Errorf("Hub.enrichSnapshot did not reach the registry: %+v", snap.Subagents[0])
	}
}
