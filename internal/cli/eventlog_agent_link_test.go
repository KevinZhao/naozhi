package cli

import (
	"sync"
	"testing"
)

// The four tests below pin RFC v4 agent-team-ui §3.2.2 / §3.3.7 state machine
// for agent team internal-view linkage:
//
//   1. applyEntryStateLocked reshapes turnAgents on agent/task_start/
//      task_progress/task_done events (drives banner + breadcrumb).
//   2. SetAgentInternalID backfills live SubagentInfo AND the ring-buffered
//      EventEntry in lock step so persistHistory flushes a complete record.
//   3. Unrelated tool_use_id on backfill is a no-op (guards against mis-wiring
//      SubagentLinker.onResolve).
//   4. Concurrent Append + SetAgentInternalID under -race (covers the "linker
//      fires after task_done" window).

func TestEventLog_AgentLifecycle_TurnAgents(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{
		Type:      "agent",
		Subagent:  "lister-1",
		TeamName:  "file-listers",
		ToolUseID: "toolu_bdrk_01A",
		Summary:   "List /tmp files",
	})

	got := l.Subagents()
	if len(got) != 1 {
		t.Fatalf("subagents = %d, want 1", len(got))
	}
	if got[0].Name != "lister-1" || got[0].Status != "spawned" {
		t.Errorf("after agent: %+v", got[0])
	}

	l.Append(EventEntry{
		Type:      "task_start",
		TaskID:    "t562ubj97",
		ToolUseID: "toolu_bdrk_01A",
		Time:      1700000000,
	})
	got = l.Subagents()
	if got[0].TaskID != "t562ubj97" || got[0].Status != "running" {
		t.Errorf("after task_start: %+v", got[0])
	}
	if got[0].StartedAtMS != 1700000000 {
		t.Errorf("StartedAtMS = %d, want 1700000000", got[0].StartedAtMS)
	}

	l.Append(EventEntry{
		Type:       "task_progress",
		TaskID:     "t562ubj97",
		LastTool:   "Bash",
		ToolUses:   3,
		DurationMS: 2100,
	})
	got = l.Subagents()
	if got[0].LastTool != "Bash" || got[0].ToolUses != 3 || got[0].DurationMS != 2100 {
		t.Errorf("after task_progress: %+v", got[0])
	}

	l.Append(EventEntry{
		Type:       "task_done",
		TaskID:     "t562ubj97",
		Status:     "completed",
		DurationMS: 2500,
	})
	got = l.Subagents()
	if got[0].Status != "completed" || got[0].DurationMS != 2500 {
		t.Errorf("after task_done: %+v", got[0])
	}

	l.Append(EventEntry{Type: "result"})
	got = l.Subagents()
	if len(got) != 0 {
		t.Errorf("result should clear turnAgents, got %d", len(got))
	}
}

func TestEventLog_AgentLifecycle_Background(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{
		Type:       "agent",
		Subagent:   "bg-scout",
		Background: true,
		ToolUseID:  "toolu_bg_01",
	})

	if got := l.BgSubagents(); len(got) != 1 || got[0].Name != "bg-scout" {
		t.Fatalf("bg subagents = %+v", got)
	}

	l.Append(EventEntry{
		Type:      "task_start",
		TaskID:    "bg_task_1",
		ToolUseID: "toolu_bg_01",
	})
	bg := l.BgSubagents()
	if bg[0].TaskID != "bg_task_1" || bg[0].Status != "running" {
		t.Errorf("bg after task_start: %+v", bg[0])
	}

	l.Append(EventEntry{
		Type:   "task_done",
		TaskID: "bg_task_1",
		Status: "error",
	})
	bg = l.BgSubagents()
	if bg[0].Status != "error" {
		t.Errorf("bg after task_done: %+v", bg[0])
	}
}

func TestEventLog_SetAgentInternalID_BackfillsLiveAndRing(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)

	l.Append(EventEntry{
		Type:      "agent",
		Subagent:  "lister-1",
		ToolUseID: "toolu_A",
	})
	l.Append(EventEntry{
		Type:      "task_start",
		TaskID:    "t1",
		ToolUseID: "toolu_A",
	})

	l.SetAgentInternalID("toolu_A", "agent-0123456789abcdef0", "/tmp/sub/agent-0123456789abcdef0.jsonl", "prompt_X")

	subs := l.Subagents()
	if len(subs) != 1 || subs[0].InternalAgentID != "agent-0123456789abcdef0" {
		t.Errorf("live SubagentInfo not backfilled: %+v", subs)
	}

	ents := l.Entries()
	var agentEnt, taskEnt *EventEntry
	for i := range ents {
		switch ents[i].Type {
		case "agent":
			agentEnt = &ents[i]
		case "task_start":
			taskEnt = &ents[i]
		}
	}
	if agentEnt == nil || taskEnt == nil {
		t.Fatalf("missing entries: %+v", ents)
	}
	for _, e := range []*EventEntry{agentEnt, taskEnt} {
		if e.InternalAgentID != "agent-0123456789abcdef0" {
			t.Errorf("%s entry InternalAgentID=%q", e.Type, e.InternalAgentID)
		}
		if e.JSONLPath != "/tmp/sub/agent-0123456789abcdef0.jsonl" {
			t.Errorf("%s entry JSONLPath=%q", e.Type, e.JSONLPath)
		}
		if e.FirstPromptID != "prompt_X" {
			t.Errorf("%s entry FirstPromptID=%q", e.Type, e.FirstPromptID)
		}
	}
}

func TestEventLog_SetAgentInternalID_UnknownToolUseID_NoOp(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)
	l.Append(EventEntry{Type: "agent", Subagent: "lister-1", ToolUseID: "toolu_A"})

	// Distinct id — nothing should change.
	l.SetAgentInternalID("toolu_B", "agent-feedfeedfeedfeedf", "/x.jsonl", "p")

	subs := l.Subagents()
	if subs[0].InternalAgentID != "" {
		t.Errorf("unrelated id leaked into SubagentInfo: %+v", subs[0])
	}
	for _, e := range l.Entries() {
		if e.InternalAgentID != "" {
			t.Errorf("unrelated id leaked into entry: %+v", e)
		}
	}
}

func TestEventLog_SetAgentInternalID_ConcurrentWithAppend(t *testing.T) {
	t.Parallel()
	l := NewEventLog(200)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			l.Append(EventEntry{
				Type:      "agent",
				Subagent:  "a",
				ToolUseID: "toolu_A",
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			l.SetAgentInternalID("toolu_A", "agent-aaaaaaaaaaaaaaaaa", "/p.jsonl", "pid")
		}
	}()
	wg.Wait()

	// No assertion beyond "did not race or panic". -race catches torn writes.
	_ = l.Entries()
	_ = l.Subagents()
}
