package session

// TestProcessLifecycle_SubsetOfProcessIface pins the R176-ARCH-M2 (#430)
// facet split: the new ProcessLifecycle interface (Alive / IsRunning /
// Close / Kill) is a strict subset of the processIface god-interface, so
// every processIface implementation (production *cli.Process, the
// testutil.TestProcess fake) also satisfies ProcessLifecycle and narrow
// consumers can switch to the smaller seam without an adapter.
//
// The compile-time `var _ ProcessLifecycle = (processIface)(nil)` in
// managed.go covers the language-level invariant; this file pins it as a
// runnable `go test` target so a method rename/removal that breaks the
// embedding shows up in CI output, not only at first build of a dependent
// package. Mirrors process_event_reader_test.go.

import "testing"

func TestProcessLifecycle_SubsetOfProcessIface(t *testing.T) {
	t.Parallel()
	// processIface is unexported; NewTestProcess returns a value that
	// satisfies it. If the facet split ever drops a method, the narrowing
	// assignment below stops compiling.
	var proc processIface = NewTestProcess()
	var lc ProcessLifecycle = proc
	// Reach through the narrow seam so the variable is load-bearing.
	// A fresh test process is not running; both calls must dispatch.
	if lc.IsRunning() {
		t.Error("fresh TestProcess should not report IsRunning")
	}
	_ = lc.Alive()
}
