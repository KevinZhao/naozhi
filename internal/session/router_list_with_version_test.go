package session

import "testing"

// TestListSessionsWithVersion_PairsAtomically pins R246-PERF-15 (#726):
// the dashboard's /api/sessions handler used to call Version() and
// ListSessions() in two separate critical sections, opening a small
// race where a mutation landing between the reads could publish data
// tagged with a stale version (or vice versa). The new tuple method
// reads both inside a single r.mu.RLock epoch so callers always see a
// (snapshots, version) pair where the version is exactly the one that
// produced the snapshot slice.
func TestListSessionsWithVersion_PairsAtomically(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})

	snaps, v0 := r.ListSessionsWithVersion()
	if len(snaps) != 0 {
		t.Fatalf("empty router: snapshots = %d, want 0", len(snaps))
	}
	// Version() reads the same atomic counter — must agree.
	if got := r.Version(); got != v0 {
		t.Errorf("Version() = %d, ListSessionsWithVersion version = %d (must match for unmutated router)", got, v0)
	}

	// BumpVersion advances storeGen exactly once; the next call must
	// return v0+1 paired with the (still empty) snapshot slice.
	r.BumpVersion()
	_, v1 := r.ListSessionsWithVersion()
	if v1 != v0+1 {
		t.Errorf("after one BumpVersion: version = %d, want %d", v1, v0+1)
	}
}

// TestListSessions_DelegatesToWithVersion pins that the legacy
// ListSessions() shape (no version) still produces the same snapshot
// slice as the tuple variant — the public API was preserved for
// callers that don't need the version (router_cleanup, persister, etc).
func TestListSessions_DelegatesToWithVersion(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})

	legacy := r.ListSessions()
	tupleSnaps, _ := r.ListSessionsWithVersion()
	if len(legacy) != len(tupleSnaps) {
		t.Errorf("len mismatch: ListSessions=%d ListSessionsWithVersion=%d", len(legacy), len(tupleSnaps))
	}
}
