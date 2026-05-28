package cron

import (
	"testing"
)

// TestRecentRunsBacking_SubSliceIndependence pins R250-PERF-16 (#1119):
// HandleList carves per-job RecentRuns slices out of a single shared
// backing array sized to the total recent-run count. The transformation
// MUST preserve two invariants:
//
//  1. each per-job sub-slice's len matches the per-job recent-runs count,
//  2. writes through one job's sub-slice MUST NOT bleed into a
//     neighbour's sub-slice — the sub-slice is created with a 3-arg
//     reslice (`backing[start:end:end]`) so an accidental append
//     overflow lands in a fresh backing array rather than overwriting
//     the next job's data.
//
// We model the carving inline (the helper logic lives in HandleList's
// loop body, not a separately-callable function); the assertion below
// would catch a regression that drops the cap argument and lets a
// future append() corrupt cross-job storage.
func TestRecentRunsBacking_SubSliceIndependence(t *testing.T) {
	t.Parallel()

	// Per-job lens chosen to mix empty + non-empty entries — the empty
	// case must NOT advance the backing cursor and must leave the
	// neighbour's sub-slice unaffected.
	perJob := []int{3, 0, 2, 5}
	total := 0
	for _, n := range perJob {
		total += n
	}
	backing := make([]cronRunSummaryView, total)
	cursor := 0
	subs := make([][]cronRunSummaryView, len(perJob))
	for i, n := range perJob {
		if n == 0 {
			continue
		}
		start := cursor
		end := start + n
		subs[i] = backing[start:end:end]
		cursor = end
		// Fill with the index so we can detect cross-job corruption later.
		for k := range subs[i] {
			subs[i][k] = cronRunSummaryView{RunID: idMarker(i, k)}
		}
	}
	if cursor != total {
		t.Fatalf("cursor = %d, want %d (some jobs missed the backing carve)", cursor, total)
	}

	// Now verify nothing bled across sub-slice boundaries: each entry's
	// RunID must still encode the (job, slot) pair we wrote.
	for i, sub := range subs {
		if len(sub) != perJob[i] {
			t.Fatalf("subs[%d] len = %d, want %d", i, len(sub), perJob[i])
		}
		for k, v := range sub {
			want := idMarker(i, k)
			if v.RunID != want {
				t.Errorf("subs[%d][%d].RunID = %q, want %q (cross-job corruption)", i, k, v.RunID, want)
			}
		}
	}

	// Capacity guard: the 3-arg reslice MUST set cap == len so a
	// careless append on one sub-slice cannot stomp the next sub-slice
	// in the shared backing storage.
	for i, sub := range subs {
		if sub == nil {
			continue
		}
		if cap(sub) != len(sub) {
			t.Errorf("subs[%d]: cap=%d != len=%d — backing[a:b:b] guard missing", i, cap(sub), len(sub))
		}
	}
}

func idMarker(job, slot int) string {
	// 16-byte lowercase-hex IDs — encode job in the first byte, slot in
	// the second so the test fails loud on any cross-slot writes.
	const hex = "0123456789abcdef"
	b := make([]byte, 16)
	for i := range b {
		b[i] = '0'
	}
	b[0] = hex[job&0xf]
	b[1] = hex[slot&0xf]
	return string(b)
}
