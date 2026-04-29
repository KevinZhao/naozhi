package session

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestLogSystemEvent_AppendsToPersistedHistory covers the proc=nil branch:
// a suspended session must still accept system-event injection so failures
// landed while the CLI is being respawned still reach the dashboard.
// This is the R49-REL-CONNECTOR-SEND-RESULT-LOSS happy path — upstream/
// connector calls sess.LogSystemEvent after sess.Send errors.
func TestLogSystemEvent_AppendsToPersistedHistory(t *testing.T) {
	s := &ManagedSession{key: "test:key"}

	s.LogSystemEvent("发送失败：connection refused")

	entries := s.EventEntries()
	if len(entries) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Type != "system" {
		t.Errorf("Type = %q, want %q", e.Type, "system")
	}
	if e.Summary != "发送失败：connection refused" {
		t.Errorf("Summary = %q, want containing the error text", e.Summary)
	}
	if e.Time == 0 {
		t.Error("Time is zero; LogSystemEvent must stamp time.Now().UnixMilli()")
	}
}

// TestLogSystemEvent_EmptySummaryIsNoop rejects blank summaries so a
// programmer error (e.g. err.Error() returning "" on a nil-wrapped error)
// does not pollute the event stream with empty system rows that the
// dashboard would render as a blank gear icon.
func TestLogSystemEvent_EmptySummaryIsNoop(t *testing.T) {
	s := &ManagedSession{key: "test:key"}

	s.LogSystemEvent("")

	if entries := s.EventEntries(); len(entries) != 0 {
		t.Errorf("LogSystemEvent(\"\") appended %d entries; want 0 (no-op)", len(entries))
	}
}

// TestLogSystemEvent_MultipleCallsAllLand confirms every non-empty call
// produces a separate entry. The connector's error path may fire several
// times in quick succession (retry loop or shim flap), and each failure
// should be visible to the operator.
func TestLogSystemEvent_MultipleCallsAllLand(t *testing.T) {
	s := &ManagedSession{key: "test:key"}

	s.LogSystemEvent("first failure")
	s.LogSystemEvent("second failure")
	s.LogSystemEvent("third failure")

	entries := s.EventEntries()
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	for i, want := range []string{"first", "second", "third"} {
		if !strings.Contains(entries[i].Summary, want) {
			t.Errorf("entry[%d].Summary = %q, want containing %q",
				i, entries[i].Summary, want)
		}
	}
}

// TestLogSystemEvent_DoesNotOverwriteLivePrompt verifies the same
// "only set if empty" guard InjectHistory uses: system events neither
// match the user/tool_use/thinking scan nor should they clobber existing
// lastPrompt / lastActivity (a pending user message set by Send).
func TestLogSystemEvent_DoesNotOverwriteLivePrompt(t *testing.T) {
	s := &ManagedSession{key: "test:key"}
	s.lastPrompt.Store("live user message")
	s.lastActivity.Store("live tool")

	s.LogSystemEvent("retry failed")

	// System events should not feed the prompt/activity scan — those
	// fields must remain what Send wrote.
	if got := loadStringOrEmpty(&s.lastPrompt); got != "live user message" {
		t.Errorf("lastPrompt = %q, want to remain \"live user message\"", got)
	}
	if got := loadStringOrEmpty(&s.lastActivity); got != "live tool" {
		t.Errorf("lastActivity = %q, want to remain \"live tool\"", got)
	}

	// But the event must still be visible to dashboard subscribers.
	entries := s.EventEntries()
	if len(entries) != 1 || entries[0].Type != "system" {
		t.Errorf("expected 1 system event in persistedHistory, got %+v", entries)
	}
}

// TestLogSystemEvent_BoundedByMaxPersistedHistory confirms the system-
// event writer respects the same cap as InjectHistory. A flood of
// connector errors must not be able to balloon persistedHistory past
// maxPersistedHistory entries.
func TestLogSystemEvent_BoundedByMaxPersistedHistory(t *testing.T) {
	s := &ManagedSession{key: "test:key"}

	// Pre-fill with maxPersistedHistory-1 non-system entries so the cap
	// is hit exactly after the next LogSystemEvent call.
	filler := make([]cli.EventEntry, maxPersistedHistory-1)
	for i := range filler {
		filler[i] = cli.EventEntry{Type: "text", Summary: "fill", Time: int64(i + 1)}
	}
	s.InjectHistory(filler)

	// Now append N more system events to force eviction of the oldest.
	for i := 0; i < 10; i++ {
		s.LogSystemEvent("oom-test")
	}

	entries := s.EventEntries()
	if len(entries) > maxPersistedHistory {
		t.Errorf("persistedHistory size %d exceeds maxPersistedHistory %d "+
			"— LogSystemEvent must honour the InjectHistory cap",
			len(entries), maxPersistedHistory)
	}
}
