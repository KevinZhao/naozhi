package session

// TestProcessEventReader_SubsetOfProcessIface is a compile-pinned guarantee
// that the new R20260527122801-ARCH-10 (#1319) facet split — narrowing the
// processIface god-interface into ProcessEventReader plus future facets —
// remains back-compat: every processIface implementation also satisfies
// ProcessEventReader, so consumers can switch to the narrower seam without
// adding adapters.
//
// The compile-time check on managed.go (var _ ProcessEventReader = ...)
// covers the language-level invariant. This file pins it as a runnable
// `go test` target so a method rename / removal that drops the embedding
// surfaces in CI output rather than only at first build of dependent
// packages.

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestProcessEventReader_SubsetOfProcessIface(t *testing.T) {
	t.Parallel()
	// processIface is unexported, so use a value of type cli.Process via
	// the exported test fake (NewTestProcess returns one that satisfies
	// processIface). If the facet split ever drops a method, the
	// downcast-via-var below stops compiling.
	var proc processIface = NewTestProcess()
	var reader ProcessEventReader = proc
	// Reach through the reader so the variable is load-bearing — otherwise
	// `var _` patterns silently pass even when nothing observes the value.
	if got := reader.EventEntries(); got == nil {
		// nil is fine for a fresh process; just ensure the call dispatches.
		_ = got
	}
	_ = []cli.EventEntry(nil) // anchor the cli import for forward-compat
}
