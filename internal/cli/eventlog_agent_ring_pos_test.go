package cli

import (
	"testing"
)

// R260528-PERF-22 (#1360) regression coverage. The sidecar must:
//
//  1. Pin both the "agent" and "task_start" ring slots after they are
//     appended, so a subsequent SetAgentInternalID resolves in O(1).
//  2. Survive the existing scan fallback when the sidecar is missing
//     the entry (e.g. a turn-boundary reset cleared the map between
//     append and resolve — rare but legal because the legacy reverse
//     scan is still authoritative).
//  3. Reset alongside taskIndex/toolUseIndex on result/user so a stale
//     ring index from the previous turn cannot leak into a fresh
//     resolve targeting the same ToolUseID string.

func TestEventLog_AgentRingPos_Sidecar_PopulatedOnAppend(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{Type: "agent", Subagent: "a1", ToolUseID: "toolu_X"})
	l.Append(EventEntry{Type: "task_start", TaskID: "t_X", ToolUseID: "toolu_X"})

	l.mu.RLock()
	pos, ok := l.agentRingByToolUse["toolu_X"]
	l.mu.RUnlock()
	if !ok {
		t.Fatalf("agentRingByToolUse missing toolu_X")
	}
	if pos.agentIdx < 0 || pos.taskStartIdx < 0 {
		t.Fatalf("agentRingPos not fully populated: %+v", pos)
	}
	if pos.agentIdx == pos.taskStartIdx {
		t.Errorf("agent and task_start mapped to the same slot: %+v", pos)
	}
}

func TestEventLog_AgentRingPos_FastPathBackfill(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{Type: "agent", Subagent: "a1", ToolUseID: "toolu_Y"})
	l.Append(EventEntry{Type: "task_start", TaskID: "t_Y", ToolUseID: "toolu_Y"})

	// Sanity: sidecar present so SetAgentInternalID hits the fast path.
	l.SetAgentInternalID("toolu_Y", "agent-aaaaaaaaaaaaaaaaa", "/p/Y.jsonl", "p_Y")

	var sawAgent, sawTaskStart bool
	for _, e := range l.Entries() {
		if e.ToolUseID != "toolu_Y" {
			continue
		}
		switch e.Type {
		case "agent":
			sawAgent = true
		case "task_start":
			sawTaskStart = true
		}
		if e.InternalAgentID != "agent-aaaaaaaaaaaaaaaaa" || e.JSONLPath != "/p/Y.jsonl" || e.FirstPromptID != "p_Y" {
			t.Errorf("%s entry not backfilled: %+v", e.Type, e)
		}
	}
	if !sawAgent || !sawTaskStart {
		t.Errorf("missing entries: agent=%v taskStart=%v", sawAgent, sawTaskStart)
	}
}

func TestEventLog_AgentRingPos_FallbackScanWhenSidecarCleared(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{Type: "agent", Subagent: "a1", ToolUseID: "toolu_Z"})
	l.Append(EventEntry{Type: "task_start", TaskID: "t_Z", ToolUseID: "toolu_Z"})

	// Simulate a stale-sidecar state: clear the map but leave ring
	// entries intact. SetAgentInternalID must still backfill via the
	// bounded reverse-scan fallback.
	l.mu.Lock()
	l.agentRingByToolUse = nil
	l.mu.Unlock()

	l.SetAgentInternalID("toolu_Z", "agent-bbbbbbbbbbbbbbbbb", "/p/Z.jsonl", "p_Z")

	for _, e := range l.Entries() {
		if e.ToolUseID == "toolu_Z" && (e.Type == "agent" || e.Type == "task_start") {
			if e.InternalAgentID != "agent-bbbbbbbbbbbbbbbbb" {
				t.Errorf("fallback scan missed entry: %+v", e)
			}
		}
	}
}

func TestEventLog_AgentRingPos_ResetOnResultBoundary(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{Type: "agent", Subagent: "a1", ToolUseID: "toolu_W"})
	l.Append(EventEntry{Type: "task_start", TaskID: "t_W", ToolUseID: "toolu_W"})

	l.Append(EventEntry{Type: "result"})

	l.mu.RLock()
	_, ok := l.agentRingByToolUse["toolu_W"]
	mapLen := len(l.agentRingByToolUse)
	l.mu.RUnlock()
	if ok {
		t.Errorf("agentRingByToolUse leaked across turn boundary")
	}
	if mapLen != 0 {
		t.Errorf("agentRingByToolUse not cleared, len=%d", mapLen)
	}
}

func TestEventLog_AgentRingPos_IgnoresEmptyToolUseID(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{Type: "agent", Subagent: "a1", ToolUseID: ""})
	l.Append(EventEntry{Type: "task_start", TaskID: "t_E", ToolUseID: ""})

	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.agentRingByToolUse[""]; ok {
		t.Errorf("empty ToolUseID polluted the sidecar")
	}
}

func TestEventLog_AgentRingPos_IgnoresOtherEntryTypes(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{Type: "tool_use", ToolUseID: "toolu_unrelated"})
	l.Append(EventEntry{Type: "tool_result", ToolUseID: "toolu_unrelated"})

	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.agentRingByToolUse["toolu_unrelated"]; ok {
		t.Errorf("non-agent/task_start entries leaked into sidecar")
	}
}
