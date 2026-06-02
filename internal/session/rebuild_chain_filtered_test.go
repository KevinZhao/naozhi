package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestRebuildChainFiltered_RemovesMaskedKeepsAligned: a mixed chain, drop the
// middle entries, verify IDs and origins stay positionally aligned.
func TestRebuildChainFiltered_RemovesMaskedKeepsAligned(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:key"}
	s.prevSessionIDs = []string{"a", "b", "c", "d"}
	s.prevSessionOrigins = []string{"manual", "auto-spawn", "auto-spawn", "manual"}

	// keep a and d (drop the two auto-spawn).
	removed := s.RebuildChainFiltered([]bool{true, false, false, true})
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	gotIDs := s.SnapshotPrevSessionIDs()
	gotOrigins := s.SnapshotPrevSessionOrigins()
	if len(gotIDs) != 2 || gotIDs[0] != "a" || gotIDs[1] != "d" {
		t.Errorf("ids = %v, want [a d]", gotIDs)
	}
	if len(gotOrigins) != 2 || gotOrigins[0] != "manual" || gotOrigins[1] != "manual" {
		t.Errorf("origins = %v, want [manual manual]", gotOrigins)
	}
	if len(gotIDs) != len(gotOrigins) {
		t.Errorf("ids/origins length mismatch: %d vs %d", len(gotIDs), len(gotOrigins))
	}
}

// TestRebuildChainFiltered_AllMaskedClearsChain.
func TestRebuildChainFiltered_AllMaskedClearsChain(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:key"}
	s.prevSessionIDs = []string{"a", "b"}
	s.prevSessionOrigins = []string{"auto-spawn", "auto-backfill"}

	removed := s.RebuildChainFiltered([]bool{false, false})
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if got := s.SnapshotPrevSessionIDs(); got != nil {
		t.Errorf("ids = %v, want nil", got)
	}
	if got := s.SnapshotPrevSessionOrigins(); got != nil {
		t.Errorf("origins = %v, want nil", got)
	}
}

// TestRebuildChainFiltered_NoneMaskedIsNoop: all-true mask leaves the chain
// untouched and reports zero removed.
func TestRebuildChainFiltered_NoneMaskedIsNoop(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:key"}
	s.prevSessionIDs = []string{"a", "b"}
	s.prevSessionOrigins = []string{"manual", "manual"}

	if removed := s.RebuildChainFiltered([]bool{true, true}); removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if got := s.SnapshotPrevSessionIDs(); len(got) != 2 {
		t.Errorf("ids = %v, want length 2", got)
	}
}

// TestRebuildChainFiltered_LengthMismatchIsSafeNoop: a wrong-length mask must
// not corrupt the chain.
func TestRebuildChainFiltered_LengthMismatchIsSafeNoop(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:key"}
	s.prevSessionIDs = []string{"a", "b", "c"}
	s.prevSessionOrigins = []string{"manual", "manual", "manual"}

	if removed := s.RebuildChainFiltered([]bool{true, false}); removed != 0 {
		t.Errorf("removed = %d, want 0 on length mismatch", removed)
	}
	if got := s.SnapshotPrevSessionIDs(); len(got) != 3 {
		t.Errorf("chain mutated on length mismatch: %v", got)
	}
}

// TestRebuildChainFiltered_NoDriftMetricFired pins the BLOCKING-2 contract:
// rebuilding must NOT trip the origins-length-mismatch drift detector, because
// the rebuild keeps the two slices aligned in one lock hold (unlike the
// Replace+Set two-call composition it replaces).
func TestRebuildChainFiltered_NoDriftMetricFired(t *testing.T) {
	// Not parallel: reads a process-global expvar counter.
	before := metrics.AutoChainOriginsLengthMismatch.Value()

	s := &ManagedSession{key: "test:key"}
	s.prevSessionIDs = []string{"a", "b", "c", "d", "e"}
	s.prevSessionOrigins = []string{"manual", "auto-spawn", "auto-spawn", "auto-backfill", "manual"}

	s.RebuildChainFiltered([]bool{true, false, false, false, true})

	// A subsequent SetPrevSessionOrigins-style read path must see aligned
	// slices. Snapshot triggers the positional fallback but not the drift
	// metric (that only fires inside SetPrevSessionOrigins). Assert the
	// counter did not move as a result of our rebuild.
	if after := metrics.AutoChainOriginsLengthMismatch.Value(); after != before {
		t.Errorf("drift metric fired: before=%d after=%d", before, after)
	}
	// Sanity: result is aligned.
	if len(s.SnapshotPrevSessionIDs()) != len(s.SnapshotPrevSessionOrigins()) {
		t.Errorf("post-rebuild slices misaligned")
	}
}

// TestRebuildChainFiltered_ShorterOriginsFallbackManual: legacy chains where
// origins is shorter than IDs — surviving untracked entries become "manual".
func TestRebuildChainFiltered_ShorterOriginsFallbackManual(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:key"}
	s.prevSessionIDs = []string{"a", "b", "c"}
	s.prevSessionOrigins = []string{"auto-spawn"} // only index 0 tracked

	// drop index 0 (the only auto), keep b and c (untracked → manual).
	removed := s.RebuildChainFiltered([]bool{false, true, true})
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	gotOrigins := s.SnapshotPrevSessionOrigins()
	if len(gotOrigins) != 2 || gotOrigins[0] != "manual" || gotOrigins[1] != "manual" {
		t.Errorf("origins = %v, want [manual manual]", gotOrigins)
	}
}
