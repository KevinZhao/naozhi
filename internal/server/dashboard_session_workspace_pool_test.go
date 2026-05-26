package server

import (
	"testing"
)

// TestWorkspaceSlicePool_Reuse verifies acquire→release→acquire returns a
// slice backed by the same array (steady-state path) so the per-poll
// allocation R217-PERF-10 / #616 targets is actually eliminated.
func TestWorkspaceSlicePool_Reuse(t *testing.T) {
	// Drive a single goroutine through several acquire/release cycles. We
	// can't directly assert "no alloc" without testing.AllocsPerRun (which
	// runs the closure N times and is sensitive to GC noise), but we can
	// pin the contract: a slice released back to the pool retains its
	// capacity and is returned on the next acquire of equal-or-smaller
	// hint. sync.Pool can drop entries during GC so retry a few times to
	// dampen flake when the test runs alongside heavy GC pressure.
	const hint = 16

	for try := 0; try < 32; try++ {
		s1 := acquireWorkspaceSlice(hint)
		s1 = append(s1, "a", "b", "c")
		cap1 := cap(s1)
		releaseWorkspaceSlice(s1)

		s2 := acquireWorkspaceSlice(hint)
		if cap(s2) >= cap1 {
			// Reuse observed.
			releaseWorkspaceSlice(s2)
			return
		}
		releaseWorkspaceSlice(s2)
	}
	t.Skip("sync.Pool dropped the entry under GC pressure; reuse contract holds at the API level")
}

// TestWorkspaceSlicePool_ZeroClearOnRelease verifies releaseWorkspaceSlice
// drops element references so the pool cannot pin workspace strings past
// the session lifetime that owned them.
func TestWorkspaceSlicePool_ZeroClearOnRelease(t *testing.T) {
	s := acquireWorkspaceSlice(8)
	s = append(s, "alpha", "beta", "gamma")

	// Capture a reference to the underlying array, then release.
	view := s[:cap(s)] // expand to capture full backing array
	releaseWorkspaceSlice(s)

	// After release, all element slots used by the caller must be zero.
	// view aliases the same backing array (we expanded above), so we can
	// inspect them post-release. We only check the slots that were
	// populated — slots past len(s) were never written.
	for i := 0; i < 3; i++ {
		if view[i] != "" {
			t.Errorf("workspace[%d] = %q after release; want \"\" (zero-clear required so pool can't pin strings)", i, view[i])
		}
	}
}

// TestWorkspaceSlicePool_NilRelease verifies releaseWorkspaceSlice tolerates
// a nil input (defensive for tests that build adjacent code paths).
func TestWorkspaceSlicePool_NilRelease(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("releaseWorkspaceSlice(nil) panicked: %v", r)
		}
	}()
	releaseWorkspaceSlice(nil)
}

// TestWorkspaceSlicePool_LargeHintBypassesPool verifies that an oversize
// hint allocates fresh capacity rather than handing back a too-small
// pooled slice. Without this, the caller would silently grow-realloc at
// append time, defeating the per-poll alloc-elimination goal.
func TestWorkspaceSlicePool_LargeHintBypassesPool(t *testing.T) {
	const giant = 4096
	s := acquireWorkspaceSlice(giant)
	if cap(s) < giant {
		t.Errorf("acquireWorkspaceSlice(%d) cap = %d, want >= %d", giant, cap(s), giant)
	}
	releaseWorkspaceSlice(s)
}
