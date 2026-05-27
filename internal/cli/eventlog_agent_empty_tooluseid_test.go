package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestEventLog_AgentEntry_EmptyToolUseID_Warns pins R20260527-COR-14 (#1296):
// when an "agent" entry arrives with empty ToolUseID, applyEntryStateLocked
// still appends it to turnAgents/bgAgents (production behavior preserved) but
// emits a slog.Warn so the upstream emitter that dropped ToolUseID can be
// diagnosed. Without ToolUseID the subsequent task_start match guard
// (`l.turnAgents[i].ToolUseID != ""`) silently drops the linkage and the
// agent stays stuck at Status="spawned".
func TestEventLog_AgentEntry_EmptyToolUseID_Warns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := NewEventLog(8)
	l.Append(EventEntry{
		Type:     "agent",
		Subagent: "orphan-agent",
		// ToolUseID intentionally empty
		TaskType: "Task",
	})

	if got := l.Subagents(); len(got) != 1 {
		t.Fatalf("subagents = %d, want 1 (entry must still be appended)", len(got))
	}

	out := buf.String()
	if !strings.Contains(out, "missing ToolUseID") {
		t.Errorf("expected slog.Warn about missing ToolUseID, got:\n%s", out)
	}
	if !strings.Contains(out, "orphan-agent") {
		t.Errorf("expected warn to include agent name, got:\n%s", out)
	}
}

// TestEventLog_AgentEntry_NonEmptyToolUseID_NoWarn pins the negative case:
// the warn must NOT fire when ToolUseID is present (the production hot path).
func TestEventLog_AgentEntry_NonEmptyToolUseID_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := NewEventLog(8)
	l.Append(EventEntry{
		Type:      "agent",
		Subagent:  "good-agent",
		ToolUseID: "toolu_01XYZ",
	})

	if strings.Contains(buf.String(), "missing ToolUseID") {
		t.Errorf("did not expect missing-ToolUseID warn, got:\n%s", buf.String())
	}
}
