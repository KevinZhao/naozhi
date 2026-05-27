package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestApplyEntryStateLocked_EmptyToolUseID_WarnLogged pins R20260527-COR-14
// (#1296): agent entries with an empty ToolUseID can never be linked by
// task_start (whose match gate requires ToolUseID != ""), so the EventLog
// must emit slog.Warn so the orphaned-agent anomaly is observable. Before
// the fix the anomaly was silent and the spawned->running advancement
// simply never fired.
func TestApplyEntryStateLocked_EmptyToolUseID_WarnLogged(t *testing.T) {
	// No t.Parallel: production code logs via slog.Default(); we install a
	// per-test JSONHandler over the package default. With t.Parallel another
	// concurrent cli test (e.g. TestEventLog_TurnAgents) writes through the
	// same default and races on the captured bytes.Buffer.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := NewEventLog(0)
	l.Append(EventEntry{
		Type:      "agent",
		Subagent:  "code-reviewer",
		Summary:   "review-batch-1",
		ToolUseID: "", // missing — anomaly
	})

	out := buf.String()
	if !strings.Contains(out, "agent entry missing ToolUseID") {
		t.Fatalf("expected slog.Warn for empty ToolUseID, got log buffer:\n%s", out)
	}

	// Sanity-check the entry was still appended (the warn does NOT short
	// circuit the append — production today silently fails to link, the
	// fix is purely diagnostic).
	if c := l.turnAgentCount.Load(); c != 1 {
		t.Errorf("turnAgentCount = %d, want 1 (agent entry should still be appended)", c)
	}
}

// TestApplyEntryStateLocked_NonEmptyToolUseID_NoWarn locks the negative case:
// agents with a populated ToolUseID must NOT trigger the warn, otherwise
// every healthy turn floods journald.
func TestApplyEntryStateLocked_NonEmptyToolUseID_NoWarn(t *testing.T) {
	// No t.Parallel: see TestApplyEntryStateLocked_EmptyToolUseID_WarnLogged.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := NewEventLog(0)
	l.Append(EventEntry{
		Type:      "agent",
		Subagent:  "code-reviewer",
		Summary:   "review-batch-1",
		ToolUseID: "tool_abc123",
	})

	if strings.Contains(buf.String(), "agent entry missing ToolUseID") {
		t.Fatalf("warn fired with non-empty ToolUseID:\n%s", buf.String())
	}
}
