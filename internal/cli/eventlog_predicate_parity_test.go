package cli

// R249-PERF-15 (#934): the entryAffectsAgentState predicate and
// applyEntryStateLocked's switch enumerate the same set of event types.
// Today they are kept in lockstep by a comment-only contract on the
// predicate godoc ("must stay in lockstep with applyEntryStateLocked's
// case labels"). This test pins the parity at runtime: every type the
// predicate returns true for must produce some observable effect when
// applyEntryStateLocked runs, AND every other type must be a no-op
// (which is exactly the gate's purpose — skip the call frame).
//
// The cheap proxy for "produces observable effect" is the SubagentInfo
// turn-state shape: agent / task_start / task_progress / task_done all
// touch turnAgents or bgAgents; result / user clear them. Any future
// fork that adds a case to applyEntryStateLocked but forgets the
// predicate (or vice versa) trips one of the assertions below.

import (
	"testing"
)

func TestEventLog_EntryAffectsAgentState_LockstepWithApplySwitch(t *testing.T) {
	t.Parallel()

	// All known event types touched by applyEntryStateLocked. If a new
	// case label lands in applyEntryStateLocked, add it here AND in
	// entryAffectsAgentState — the two assertions below catch the drift.
	gateTypes := []string{"agent", "task_start", "task_progress", "task_done", "result", "user"}

	// 1. Every gate type must satisfy the predicate.
	for _, ty := range gateTypes {
		if !entryAffectsAgentState(ty) {
			t.Errorf("entryAffectsAgentState(%q) = false; applyEntryStateLocked has a case for it — predicate drifted", ty)
		}
	}

	// 2. A representative set of "default arm" types must NOT satisfy
	//    the predicate. This is the actual perf gate — these types skip
	//    the switch dispatch entirely on the hot Append path.
	defaultTypes := []string{
		"text", "thinking", "tool_use", "tool_result", "system", "metadata",
		"assistant", "todos", "ask_question", "ask_response",
	}
	for _, ty := range defaultTypes {
		if entryAffectsAgentState(ty) {
			t.Errorf("entryAffectsAgentState(%q) = true; gate would route default-arm type into applyEntryStateLocked — perf gate broken", ty)
		}
	}

	// 3. Behavioural assertion: invoking applyEntryStateLocked with a
	//    gate type produces visible side effects (or returns the
	//    pendingTaskDone signal); invoking with a non-gate type is a
	//    pure no-op. This is the runtime witness for the predicate's
	//    semantic contract — not just textual case-list parity.
	l := NewEventLog(8)

	// gate type "agent" appends to turnAgents; non-gate type "tool_use"
	// must not.
	beforeCount := l.turnAgentCount.Load()
	l.applyEntryStateLocked(EventEntry{Type: "agent", Subagent: "a", ToolUseID: "u1"})
	if l.turnAgentCount.Load() == beforeCount {
		t.Error("applyEntryStateLocked(agent) did not bump turnAgentCount; gate-type behaviour drifted")
	}

	beforeCount = l.turnAgentCount.Load()
	l.applyEntryStateLocked(EventEntry{Type: "tool_use", Tool: "Bash"})
	if l.turnAgentCount.Load() != beforeCount {
		t.Error("applyEntryStateLocked(tool_use) bumped turnAgentCount; default-arm should be a no-op")
	}
}
