package cli

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestEventEntriesFromEventAt_BackendRejectionNotice: a rejected turn has no
// assistant frame before its result, so the event log must carry a system line
// with the error or the dashboard shows the turn stopping silently.
func TestEventEntriesFromEventAt_BackendRejectionNotice(t *testing.T) {
	t.Parallel()
	rejected := (&TurnRejectedError{Backend: "kiro", Code: -32603, Message: "Internal error: model unavailable",
		Err: ErrACPRPC}).resultEvent()

	entries := EventEntriesFromEventAt(rejected, 1000)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want a system notice then the result", entries)
	}
	if entries[0].Type != clievent.KindSystem || entries[0].Summary != rejected.Result {
		t.Errorf("notice = %+v, want system entry with summary %q", entries[0], rejected.Result)
	}
	if entries[1].Type != clievent.KindResult {
		t.Errorf("last entry type = %q, want result", entries[1].Type)
	}

	// A healthy result (its text came in an assistant frame) adds nothing.
	ok := EventEntriesFromEventAt(clievent.Event{Type: "result", Result: "done"}, 1000)
	if len(ok) != 1 || ok[0].Type != clievent.KindResult {
		t.Errorf("healthy result entries = %+v, want the result alone", ok)
	}
}
