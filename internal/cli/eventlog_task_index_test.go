package cli

import (
	"strconv"
	"testing"
)

// R260528-PERF-6 (#1353): the taskIndex sidecar must (a) be seeded on
// task_start, (b) make task_progress/task_done O(1) hits, (c) reset on
// result so the next turn rebuilds from scratch, and (d) survive a
// fan-out larger than the typical-turn retain cap.

func TestEventLog_TaskIndex_FanoutSeedAndReset(t *testing.T) {
	t.Parallel()
	l := NewEventLog(200)

	// Spawn 16 foreground subagents (TeamCreate fan-out scale).
	const n = 16
	for i := 0; i < n; i++ {
		toolUse := "toolu_" + strconv.Itoa(i)
		l.Append(EventEntry{Type: "agent", Subagent: "ag" + strconv.Itoa(i), ToolUseID: toolUse})
		l.Append(EventEntry{Type: "task_start", TaskID: "task_" + strconv.Itoa(i), ToolUseID: toolUse, Time: 1700000000})
	}

	// Sidecar should now hold exactly n entries (one per task_start).
	l.mu.Lock()
	if got := len(l.taskIndex); got != n {
		l.mu.Unlock()
		t.Fatalf("taskIndex size after fan-out = %d, want %d", got, n)
	}
	// And every entry must point at a slot whose TaskID still matches.
	for tid, ref := range l.taskIndex {
		if ref.background {
			t.Fatalf("foreground fan-out leaked into bgAgents ref: %s -> %+v", tid, ref)
		}
		if ref.index >= len(l.turnAgents) || l.turnAgents[ref.index].TaskID != tid {
			t.Fatalf("stale ref for %s: %+v turnAgents[%d]=%+v", tid, ref, ref.index, l.turnAgents[ref.index])
		}
	}
	l.mu.Unlock()

	// task_progress + task_done should land via the O(1) path on every entry.
	for i := 0; i < n; i++ {
		l.Append(EventEntry{Type: "task_progress", TaskID: "task_" + strconv.Itoa(i), LastTool: "Bash", ToolUses: i + 1})
	}
	got := l.Subagents()
	if len(got) != n {
		t.Fatalf("snapshot size = %d, want %d", len(got), n)
	}
	for i, info := range got {
		want := i + 1
		if info.ToolUses != want || info.LastTool != "Bash" {
			t.Errorf("task_progress for %s did not land: %+v", info.TaskID, info)
		}
	}

	for i := 0; i < n; i++ {
		l.Append(EventEntry{Type: "task_done", TaskID: "task_" + strconv.Itoa(i), Status: "completed"})
	}
	// task_done removes from taskIndex; sidecar should be empty before the
	// result-driven full reset.
	l.mu.Lock()
	if len(l.taskIndex) != 0 {
		l.mu.Unlock()
		t.Fatalf("taskIndex after all task_done = %d, want 0", len(l.taskIndex))
	}
	l.mu.Unlock()

	l.Append(EventEntry{Type: "result"})
	if got := l.Subagents(); len(got) != 0 {
		t.Fatalf("result should clear turnAgents, got %d", len(got))
	}
	l.mu.Lock()
	if len(l.taskIndex) != 0 {
		l.mu.Unlock()
		t.Fatalf("taskIndex after result = %d, want 0", len(l.taskIndex))
	}
	l.mu.Unlock()
}

// task_progress arriving for a TaskID that was never seen via task_start
// must still be a no-op (not panic). Mirrors the pre-sidecar fallback path.
func TestEventLog_TaskIndex_StaleProgressNoMatch(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)
	l.Append(EventEntry{Type: "task_progress", TaskID: "ghost", LastTool: "Bash"})
	if got := l.Subagents(); len(got) != 0 {
		t.Fatalf("ghost progress should not synthesise an agent, got %+v", got)
	}
}

// Background agent must be reachable through the same sidecar.
func TestEventLog_TaskIndex_BackgroundRoute(t *testing.T) {
	t.Parallel()
	l := NewEventLog(20)
	l.Append(EventEntry{Type: "agent", Subagent: "bg", Background: true, ToolUseID: "tu_bg"})
	l.Append(EventEntry{Type: "task_start", TaskID: "bgtask", ToolUseID: "tu_bg"})
	l.Append(EventEntry{Type: "task_progress", TaskID: "bgtask", LastTool: "Read", ToolUses: 9})
	bg := l.BgSubagents()
	if len(bg) != 1 || bg[0].LastTool != "Read" || bg[0].ToolUses != 9 {
		t.Fatalf("bg route: %+v", bg)
	}
	l.mu.Lock()
	ref, ok := l.taskIndex["bgtask"]
	l.mu.Unlock()
	if !ok || !ref.background {
		t.Fatalf("taskIndex[bgtask] = %+v ok=%v, want background=true", ref, ok)
	}
}
