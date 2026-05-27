package cli

import (
	"testing"

	"github.com/naozhi/naozhi/internal/shim"
)

// TestWrapper_Manager_Accessor pins the R242-ARCH-3 (#721) forward-compat
// step: Wrapper.Manager() must return the same value as the ShimManager
// field for as long as both are exported. When the field is eventually
// unexported, this test still serves as the contract test for the
// accessor's "nil receiver returns nil" + "uninitialised wrapper returns
// nil" guarantees that callers depend on for the safe-default branch.
func TestWrapper_Manager_Accessor(t *testing.T) {
	t.Parallel()

	// nil receiver: contract is "return nil" so callers can chain
	// w.Manager() == nil checks without a separate nil-Wrapper guard.
	var nilW *Wrapper
	if got := nilW.Manager(); got != nil {
		t.Fatalf("(*Wrapper)(nil).Manager() = %v, want nil", got)
	}

	// Uninitialised wrapper (no ShimManager set): accessor returns nil,
	// matching the field's zero value.
	w := &Wrapper{}
	if got := w.Manager(); got != nil {
		t.Fatalf("Wrapper{}.Manager() = %v, want nil (unset field)", got)
	}

	// With a manager set: accessor must surface the same pointer the
	// field exposes. Pointer-equality is the right contract — the
	// accessor is not allowed to wrap or copy the manager.
	mgr := &shim.Manager{}
	w.ShimManager = mgr
	if got := w.Manager(); got != mgr {
		t.Fatalf("Wrapper.Manager() = %p, want %p (must match ShimManager field)", got, mgr)
	}
}
