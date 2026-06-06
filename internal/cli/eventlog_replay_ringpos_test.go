package cli

import (
	"testing"
)

// TestEventLog_ReplayBatch_RecordsAgentRingPos pins R20260601-PERF-9 (#1549):
// the appendBatch replay path inlines the agent/task_start type gate before
// calling recordAgentRingPosLocked. The optimization must NOT regress the
// replay sidecar contract — an InjectHistory replay of agent/task_start
// entries must still pin their ring slots so a later live SetAgentInternalID
// resolves in O(1).
func TestEventLog_ReplayBatch_RecordsAgentRingPos(t *testing.T) {
	t.Parallel()
	l := NewEventLog(50)

	l.AppendBatchReplay([]EventEntry{
		{Type: "assistant_text", Summary: "hi"},
		{Type: "agent", Subagent: "a1", ToolUseID: "toolu_R"},
		{Type: "tool_use", ToolUseID: "toolu_other"},
		{Type: "task_start", TaskID: "t_R", ToolUseID: "toolu_R"},
	})

	l.mu.RLock()
	pos, ok := l.agentRingByToolUse["toolu_R"]
	// non-agent/task_start types must NOT have been recorded.
	_, leaked := l.agentRingByToolUse["toolu_other"]
	l.mu.RUnlock()

	if !ok {
		t.Fatal("replay must pin agent/task_start ring slot for toolu_R (#1549)")
	}
	if pos.agentIdx < 0 || pos.taskStartIdx < 0 {
		t.Fatalf("replay sidecar not fully populated: %+v", pos)
	}
	if leaked {
		t.Fatal("non-agent entry toolu_other must not be recorded in sidecar")
	}
}

// TestEventLog_ReplayBatch_NonAgentNoSidecar confirms a replay batch with no
// agent/task_start rows leaves the sidecar map untouched (nil/empty) — the
// inlined gate skips recordAgentRingPosLocked entirely for the common case.
func TestEventLog_ReplayBatch_NonAgentNoSidecar(t *testing.T) {
	t.Parallel()
	l := NewEventLog(50)

	entries := make([]EventEntry, 0, 100)
	for i := 0; i < 100; i++ {
		entries = append(entries, EventEntry{Type: "assistant_text", Summary: "x"})
	}
	l.AppendBatchReplay(entries)

	l.mu.RLock()
	n := len(l.agentRingByToolUse)
	l.mu.RUnlock()
	if n != 0 {
		t.Fatalf("non-agent replay must not populate the ring-pos sidecar, got %d", n)
	}
}
