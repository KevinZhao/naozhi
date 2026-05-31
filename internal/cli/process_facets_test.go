package cli

// Runnable pins for the R245-ARCH-42 (#902) additive Process facet split:
// ProcessLifecycle / ProcessTurnIO / ProcessIntrospect are each satisfied
// by the concrete *Process, so a narrow consumer can switch to the
// smaller seam without an adapter. The `var _ Facet = (*Process)(nil)`
// pins in process_facets.go cover the language-level invariant; this file
// pins them as runnable `go test` targets so a method rename/removal that
// breaks a facet shows up in CI output, not only at first build of a
// dependent package. Mirrors process_facet_subset (#668) and the
// session-package process_lifecycle_test.go (#430).

import "testing"

func TestProcessFacets_SatisfiedByProcess(t *testing.T) {
	t.Parallel()
	// A bare *Process is enough to exercise the type-narrowing; we only
	// call methods that are safe on a zero value (no shim IO).
	p := &Process{}
	var lc ProcessLifecycle = p
	var io ProcessTurnIO = p
	var in ProcessIntrospect = p

	// Reach through each seam so the variables are load-bearing and a
	// dropped method fails the build.
	if !lc.Alive() {
		t.Error("zero-value Process should report Alive (done chan nil → default)")
	}
	if lc.IsRunning() {
		t.Error("zero-value Process should not report IsRunning")
	}
	if in.GetState() != StateSpawning {
		t.Errorf("zero-value Process GetState = %v, want StateSpawning", in.GetState())
	}
	// io is verified at compile time by the narrowing assignment above;
	// its methods do shim IO so they are not invoked on a bare Process.
	_ = io
}
