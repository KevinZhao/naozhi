package session

// TestHistoryInjector_SubsetOfProcessIface pins the R176-ARCH-M2 (#430)
// facet split: the HistoryInjector interface (InjectHistory / TurnAgents)
// is a strict subset of the processIface god-interface, so every
// processIface implementation also satisfies HistoryInjector and the
// history-replay paths can narrow to the smaller seam without an adapter.
//
// The compile-time `var _ HistoryInjector = (processIface)(nil)` in
// managed.go covers the language-level invariant; this file pins it as a
// runnable `go test` target. Mirrors process_event_reader_test.go and
// process_lifecycle_test.go.

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestHistoryInjector_SubsetOfProcessIface(t *testing.T) {
	t.Parallel()
	var proc processIface = NewTestProcess()
	var hi HistoryInjector = proc
	// Reach through the narrow seam so the assignment is load-bearing.
	// InjectHistory with an empty slice must be a no-op (the spawn path
	// always calls it, sometimes with zero prior entries).
	hi.InjectHistory(nil)
	if agents := hi.TurnAgents(); len(agents) != 0 {
		t.Errorf("fresh TestProcess should report no turn agents, got %d", len(agents))
	}
	_ = []cli.SubagentInfo(nil) // anchor the cli import for forward-compat
}
