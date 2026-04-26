package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestInjectHistory_UpdatesLastPromptAndActivity locks the semantic contract
// after R61-PERF-9 moved the prompt/activity summary scan out of historyMu:
// the scan still runs, summaries still land in lastPrompt / lastActivity, and
// the "only set if previously empty" guard still holds.
func TestInjectHistory_UpdatesLastPromptAndActivity(t *testing.T) {
	s := &ManagedSession{key: "test:key"}

	s.InjectHistory([]cli.EventEntry{
		{Type: "user", Summary: "first question"},
		{Type: "tool_use", Summary: "Read file.go"},
		{Type: "thinking", Summary: "considering"}, // older activity does not override tool_use when scanned end-to-start
		{Type: "user", Summary: "second question"},
	})

	// Scan iterates newest→oldest; latest user entry wins.
	if got, want := loadStringOrEmpty(&s.lastPrompt), "second question"; got != want {
		t.Errorf("lastPrompt = %q, want %q", got, want)
	}
	// tool_use / thinking: whichever is newest wins; here that's "thinking".
	if got, want := loadStringOrEmpty(&s.lastActivity), "considering"; got != want {
		t.Errorf("lastActivity = %q, want %q", got, want)
	}
}

// TestInjectHistory_DoesNotOverwriteNonEmpty pins the "only set if empty"
// branch — a second InjectHistory call (e.g. batched JSONL replay) must not
// clobber summaries already populated by Send.
func TestInjectHistory_DoesNotOverwriteNonEmpty(t *testing.T) {
	s := &ManagedSession{key: "test:key"}
	s.lastPrompt.Store("set by send")
	s.lastActivity.Store("live tool")

	s.InjectHistory([]cli.EventEntry{
		{Type: "user", Summary: "historical prompt"},
		{Type: "tool_use", Summary: "historical tool"},
	})

	if got, want := loadStringOrEmpty(&s.lastPrompt), "set by send"; got != want {
		t.Errorf("lastPrompt overwritten: got %q, want %q", got, want)
	}
	if got, want := loadStringOrEmpty(&s.lastActivity), "live tool"; got != want {
		t.Errorf("lastActivity overwritten: got %q, want %q", got, want)
	}
}

// TestInjectHistory_PersistsEntries covers the append side-effect: entries
// added while historyMu is briefly held must be observable by the read
// accessors (EventEntries / EventEntriesBefore) after InjectHistory returns.
func TestInjectHistory_PersistsEntries(t *testing.T) {
	s := &ManagedSession{key: "test:key"}
	s.InjectHistory([]cli.EventEntry{
		{Time: 1000, Type: "user", Summary: "q1"},
		{Time: 2000, Type: "user", Summary: "q2"},
	})

	got := s.EventEntries()
	if len(got) != 2 {
		t.Fatalf("EventEntries len = %d, want 2", len(got))
	}
	if got[0].Summary != "q1" || got[1].Summary != "q2" {
		t.Errorf("entries out of order: %+v", got)
	}
}

// TestInjectHistory_TrimsToMaxPersisted guards the cap — historyMu is held
// during the trim so concurrent readers see either pre- or post-trim state,
// never a torn slice. The slice returned by EventEntries is a copy, so length
// equality after a >maxPersistedHistory injection locks the trim behaviour.
func TestInjectHistory_TrimsToMaxPersisted(t *testing.T) {
	s := &ManagedSession{key: "test:key"}
	oversized := make([]cli.EventEntry, maxPersistedHistory+50)
	for i := range oversized {
		oversized[i] = cli.EventEntry{Time: int64(i), Type: "user", Summary: "x"}
	}
	s.InjectHistory(oversized)

	got := s.EventEntries()
	if len(got) != maxPersistedHistory {
		t.Errorf("EventEntries len = %d, want %d", len(got), maxPersistedHistory)
	}
}
