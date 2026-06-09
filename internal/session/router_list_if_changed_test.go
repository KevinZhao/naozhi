package session

import "testing"

// TestListSessionsIfChanged_ShortCircuitsWhenUnchanged pins R20260607-PERF-7
// (#1886): when the caller's sinceVersion matches the current storeGen, the
// REST-poll variant returns changed==false with a nil snapshot slice and the
// unchanged version, skipping the make([]SessionSnapshot)+Snapshot build that
// ListSessionsWithVersion always pays.
func TestListSessionsIfChanged_ShortCircuitsWhenUnchanged(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})

	// Sample the current version, then poll with it as sinceVersion: no
	// mutation happened in between, so the call must short-circuit.
	_, v := r.ListSessionsWithVersion()
	snaps, ver, changed := r.ListSessionsIfChanged(v)
	if changed {
		t.Errorf("changed = true, want false (storeGen unmoved)")
	}
	if ver != v {
		t.Errorf("version = %d, want %d (unchanged)", ver, v)
	}
	if snaps != nil {
		t.Errorf("snapshots = %v, want nil on short-circuit", snaps)
	}
}

// TestListSessionsIfChanged_ReturnsDataWhenChanged verifies that once storeGen
// advances past the caller's sinceVersion, the full (snapshots, version) pair
// is built and changed==true.
func TestListSessionsIfChanged_ReturnsDataWhenChanged(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})

	_, v0 := r.ListSessionsWithVersion()

	// A mutation bumps storeGen exactly once.
	r.BumpVersion()

	snaps, v1, changed := r.ListSessionsIfChanged(v0)
	if !changed {
		t.Fatalf("changed = false, want true after BumpVersion")
	}
	if v1 != v0+1 {
		t.Errorf("version = %d, want %d", v1, v0+1)
	}
	// Empty router: snapshot slice is empty but the call still went through
	// the full path (changed==true), matching ListSessionsWithVersion shape.
	if len(snaps) != 0 {
		t.Errorf("snapshots len = %d, want 0 (empty router)", len(snaps))
	}

	// A follow-up poll at the new version short-circuits again.
	_, v2, changed2 := r.ListSessionsIfChanged(v1)
	if changed2 {
		t.Errorf("second poll changed = true, want false (caught up)")
	}
	if v2 != v1 {
		t.Errorf("second poll version = %d, want %d", v2, v1)
	}
}

// TestListSessionsIfChanged_FirstPollFromZero verifies the cold-start contract:
// a client that has never polled passes sinceVersion=0. On a router that has
// already advanced past 0, this must report changed==true and serve data; on a
// pristine gen-0 router it correctly reports no change.
func TestListSessionsIfChanged_FirstPollFromZero(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})

	_, v0 := r.ListSessionsWithVersion()
	if v0 == 0 {
		// Pristine router: a sinceVersion=0 poll legitimately sees no change.
		if _, _, changed := r.ListSessionsIfChanged(0); changed {
			t.Errorf("gen-0 router: changed = true, want false")
		}
	}

	r.BumpVersion()
	if _, _, changed := r.ListSessionsIfChanged(0); !changed {
		t.Errorf("after bump, sinceVersion=0 poll: changed = false, want true")
	}
}
