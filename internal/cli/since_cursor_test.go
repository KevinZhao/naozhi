package cli

import "testing"

func cursorSummaries(es []EventEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Summary
	}
	return out
}

// TestSinceCursor_SameMillisecondAcrossWaves is the R20260530-GO-1 (#1481)
// regression: an entry that lands in the SAME millisecond as the tail of an
// already-delivered batch, but arrives in a later notify wave, MUST still be
// delivered. The old `EntriesSince(lastTime)` (strictly >) dropped it.
// Promoted from internal/upstream when the cursor moved here (#2402).
func TestSinceCursor_SameMillisecondAcrossWaves(t *testing.T) {
	csr := NewSinceCursor()

	// Wave 1: two entries at t=100.
	wave1 := []EventEntry{
		{Time: 100, UUID: "a", Summary: "a"},
		{Time: 100, UUID: "b", Summary: "b"},
	}
	got1 := csr.Filter(append([]EventEntry(nil), wave1...))
	if g := cursorSummaries(got1); len(g) != 2 || g[0] != "a" || g[1] != "b" {
		t.Fatalf("wave1 delivered = %v, want [a b]", cursorSummaries(got1))
	}
	csr.Advance(got1)
	if csr.Watermark() != 100 {
		t.Fatalf("watermark = %d, want 100", csr.Watermark())
	}

	// Wave 2: the store now also contains a NEW entry "c" at the same t=100.
	// EntriesSince(queryAfter == 99) re-returns the whole t=100 millisecond.
	store := []EventEntry{
		{Time: 100, UUID: "a", Summary: "a"},
		{Time: 100, UUID: "b", Summary: "b"},
		{Time: 100, UUID: "c", Summary: "c"},
	}
	if csr.QueryAfter() != 99 {
		t.Fatalf("QueryAfter = %d, want 99", csr.QueryAfter())
	}
	got2 := csr.Filter(append([]EventEntry(nil), store...))
	if g := cursorSummaries(got2); len(g) != 1 || g[0] != "c" {
		t.Fatalf("wave2 delivered = %v, want [c] (a,b already sent)", cursorSummaries(got2))
	}
	csr.Advance(got2)
	if csr.Watermark() != 100 {
		t.Fatalf("watermark after wave2 = %d, want 100", csr.Watermark())
	}

	// Wave 3: nothing new at t=100 → empty delivery, no duplicates.
	got3 := csr.Filter(append([]EventEntry(nil), store...))
	if len(got3) != 0 {
		t.Fatalf("wave3 delivered = %v, want []", cursorSummaries(got3))
	}
}

// TestSinceCursor_WatermarkAdvances confirms that once the watermark moves to
// a later millisecond, the dedup set is rebuilt and older entries no longer
// re-deliver.
func TestSinceCursor_WatermarkAdvances(t *testing.T) {
	csr := NewSinceCursor()

	w1 := []EventEntry{{Time: 100, UUID: "a", Summary: "a"}}
	csr.Advance(csr.Filter(w1))

	// New entry at t=200.
	store := []EventEntry{
		{Time: 100, UUID: "a", Summary: "a"},
		{Time: 200, UUID: "d", Summary: "d"},
	}
	got := csr.Filter(append([]EventEntry(nil), store...))
	if g := cursorSummaries(got); len(g) != 1 || g[0] != "d" {
		t.Fatalf("delivered = %v, want [d]", cursorSummaries(got))
	}
	csr.Advance(got)
	if csr.Watermark() != 200 {
		t.Fatalf("watermark = %d, want 200", csr.Watermark())
	}
	if csr.containsWM("a") {
		t.Errorf("dedup set should have been rebuilt for t=200, still holds t=100 UUID")
	}
	if !csr.containsWM("d") {
		t.Errorf("dedup set missing the t=200 UUID")
	}
}

// TestSinceCursor_NoDuplicateAccumulation guards the R164029-PERF-4 (#1599)
// slice rewrite: advancing repeatedly with the same UUID at the trailing
// millisecond must not grow sentAtWM (the old map deduped implicitly; the
// slice version must keep the same invariant via containsWM).
func TestSinceCursor_NoDuplicateAccumulation(t *testing.T) {
	csr := NewSinceCursor()
	e := []EventEntry{{Time: 300, UUID: "z", Summary: "z"}}
	csr.Advance(e)
	csr.Advance(e)
	csr.Advance(e)
	if len(csr.sentAtWM) != 1 {
		t.Fatalf("sentAtWM grew to %d entries on repeated advance, want 1", len(csr.sentAtWM))
	}
	if !csr.containsWM("z") {
		t.Errorf("sentAtWM missing UUID after advance")
	}
}

// TestSinceCursor_Reset clears the watermark and dedup set on session swap.
func TestSinceCursor_Reset(t *testing.T) {
	csr := NewSinceCursor()
	csr.Advance(csr.Filter([]EventEntry{{Time: 500, UUID: "x", Summary: "x"}}))
	csr.Reset()
	if csr.Watermark() != 0 {
		t.Fatalf("watermark after reset = %d, want 0", csr.Watermark())
	}
	if csr.QueryAfter() != -1 {
		t.Fatalf("QueryAfter after reset = %d, want -1 (deliver everything)", csr.QueryAfter())
	}
	if len(csr.sentAtWM) != 0 {
		t.Fatalf("dedup set not cleared on reset: %d entries", len(csr.sentAtWM))
	}
}
