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
		kid: knownIDsStore{ids: make(map[string]bool)},
	}

	// Insert one above the cap to exercise the eviction branch.
	total := maxKnownIDs + 5
	for i := 0; i < total; i++ {
		r.trackSessionID(fmt.Sprintf("sess-%07d", i))
	}

	if got := len(r.kid.ids); got != maxKnownIDs {
		t.Errorf("len(knownIDs) = %d, want capped at %d", got, maxKnownIDs)
	}
	// The live window is order[orderHead:]; its length tracks the map.
	if got := len(r.kid.order) - r.kid.orderHead; got != maxKnownIDs {
		t.Errorf("live window = %d, want capped at %d", got, maxKnownIDs)
	}

	// FIFO invariant: the first 5 IDs must have been evicted, and the
	// freshest insertion must be the slice tail.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("sess-%07d", i)
		if r.kid.ids[id] {
			t.Errorf("oldest ID %q was not evicted", id)
		}
	}
	want := fmt.Sprintf("sess-%07d", total-1)
	if got := r.kid.order[len(r.kid.order)-1]; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

// TestTrackSessionID_DedupesExisting verifies that re-tracking an ID
// the router has already seen does not append to knownIDsOrder, which
// would let the slice grow past maxKnownIDs even with the cap in
// place.
func TestTrackSessionID_DedupesExisting(t *testing.T) {
	r := &Router{
		kid: knownIDsStore{ids: make(map[string]bool)},
	}
	r.trackSessionID("dup")
	r.trackSessionID("dup")
	r.trackSessionID("dup")

	if got := len(r.kid.ids); got != 1 {
		t.Errorf("len(knownIDs) = %d, want 1", got)
	}
	if got := len(r.kid.order) - r.kid.orderHead; got != 1 {
		t.Errorf("live window = %d, want 1", got)
	}
}

// TestTrackSessionID_HeadCompactionBoundsMemory pins the R2188 fix: with the
// head-index eviction, inserting far more unique IDs than the cap must keep
// (a) the map capped at maxKnownIDs (FIFO correctness), (b) the order backing
// slice bounded — lazy compaction must release the dead prefix so cap(order)
// cannot grow without bound, and (c) the newest ID at the slice tail.
func TestTrackSessionID_HeadCompactionBoundsMemory(t *testing.T) {
	r := &Router{
		kid: knownIDsStore{ids: make(map[string]bool)},
	}

	total := maxKnownIDs + 20000
	for i := 0; i < total; i++ {
		r.trackSessionID(fmt.Sprintf("sess-%07d", i))
	}

	if got := len(r.kid.ids); got != maxKnownIDs {
		t.Errorf("len(knownIDs) = %d, want capped at %d", got, maxKnownIDs)
	}
	if got := len(r.kid.order) - r.kid.orderHead; got != maxKnownIDs {
		t.Errorf("live window = %d, want %d", got, maxKnownIDs)
	}
	// Compaction must keep the backing array bounded; without it the slice
	// would grow to ~total (30K) as orderHead drifted rightward forever.
	if got := cap(r.kid.order); got >= 2*maxKnownIDs {
		t.Errorf("cap(order) = %d, want bounded < %d (compaction not releasing dead prefix)", got, 2*maxKnownIDs)
	}
	want := fmt.Sprintf("sess-%07d", total-1)
	if got := r.kid.order[len(r.kid.order)-1]; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}
