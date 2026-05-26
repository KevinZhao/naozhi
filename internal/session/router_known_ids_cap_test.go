package session

import (
	"fmt"
	"testing"
)

// TestTrackSessionID_CapsAtMaxKnownIDs pins the R237-PERF-9 fix:
// knownIDsOrder must drop the oldest entry once len(knownIDs) reaches
// maxKnownIDs so the persistent set cannot grow without bound. Before
// the cap landed the slice was append-only and a long-lived process
// would keep ID strings + map keys alive indefinitely.
func TestTrackSessionID_CapsAtMaxKnownIDs(t *testing.T) {
	r := &Router{
		knownIDs: make(map[string]bool),
	}

	// Insert one above the cap to exercise the eviction branch.
	total := maxKnownIDs + 5
	for i := 0; i < total; i++ {
		r.trackSessionID(fmt.Sprintf("sess-%07d", i))
	}

	if got := len(r.knownIDs); got != maxKnownIDs {
		t.Errorf("len(knownIDs) = %d, want capped at %d", got, maxKnownIDs)
	}
	if got := len(r.knownIDsOrder); got != maxKnownIDs {
		t.Errorf("len(knownIDsOrder) = %d, want capped at %d", got, maxKnownIDs)
	}

	// FIFO invariant: the first 5 IDs must have been evicted, and the
	// freshest insertion must be the slice tail.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("sess-%07d", i)
		if r.knownIDs[id] {
			t.Errorf("oldest ID %q was not evicted", id)
		}
	}
	want := fmt.Sprintf("sess-%07d", total-1)
	if got := r.knownIDsOrder[len(r.knownIDsOrder)-1]; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

// TestTrackSessionID_DedupesExisting verifies that re-tracking an ID
// the router has already seen does not append to knownIDsOrder, which
// would let the slice grow past maxKnownIDs even with the cap in
// place.
func TestTrackSessionID_DedupesExisting(t *testing.T) {
	r := &Router{
		knownIDs: make(map[string]bool),
	}
	r.trackSessionID("dup")
	r.trackSessionID("dup")
	r.trackSessionID("dup")

	if got := len(r.knownIDs); got != 1 {
		t.Errorf("len(knownIDs) = %d, want 1", got)
	}
	if got := len(r.knownIDsOrder); got != 1 {
		t.Errorf("len(knownIDsOrder) = %d, want 1", got)
	}
}
