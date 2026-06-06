package upstream

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func summaries(es []cli.EventEntry) []string {
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
func TestSinceCursor_SameMillisecondAcrossWaves(t *testing.T) {
	csr := newSinceCursor()

	// Wave 1: two entries at t=100.
	wave1 := []cli.EventEntry{
		{Time: 100, UUID: "a", Summary: "a"},
		{Time: 100, UUID: "b", Summary: "b"},
	}
	got1 := csr.filter(append([]cli.EventEntry(nil), wave1...))
	if g := summaries(got1); len(g) != 2 || g[0] != "a" || g[1] != "b" {
		t.Fatalf("wave1 delivered = %v, want [a b]", summaries(got1))
	}
	csr.advance(got1)
	if csr.watermark != 100 {
		t.Fatalf("watermark = %d, want 100", csr.watermark)
	}

	// Wave 2: the store now also contains a NEW entry "c" at the same t=100.
	// EntriesSince(queryAfter == 99) re-returns the whole t=100 millisecond.
	store := []cli.EventEntry{
		{Time: 100, UUID: "a", Summary: "a"},
		{Time: 100, UUID: "b", Summary: "b"},
		{Time: 100, UUID: "c", Summary: "c"},
	}
	if csr.queryAfter() != 99 {
		t.Fatalf("queryAfter = %d, want 99", csr.queryAfter())
	}
	got2 := csr.filter(append([]cli.EventEntry(nil), store...))
	if g := summaries(got2); len(g) != 1 || g[0] != "c" {
		t.Fatalf("wave2 delivered = %v, want [c] (a,b already sent)", summaries(got2))
	}
	csr.advance(got2)
	if csr.watermark != 100 {
		t.Fatalf("watermark after wave2 = %d, want 100", csr.watermark)
	}

	// Wave 3: nothing new at t=100 → empty delivery, no duplicates.
	got3 := csr.filter(append([]cli.EventEntry(nil), store...))
	if len(got3) != 0 {
		t.Fatalf("wave3 delivered = %v, want []", summaries(got3))
	}
}

// TestSinceCursor_WatermarkAdvances confirms that once the watermark moves to a
// later millisecond, the dedup set is rebuilt and older entries no longer
// re-deliver.
func TestSinceCursor_WatermarkAdvances(t *testing.T) {
	csr := newSinceCursor()

	w1 := []cli.EventEntry{{Time: 100, UUID: "a", Summary: "a"}}
	csr.advance(csr.filter(w1))

	// New entry at t=200.
	store := []cli.EventEntry{
		{Time: 100, UUID: "a", Summary: "a"},
		{Time: 200, UUID: "d", Summary: "d"},
	}
	got := csr.filter(append([]cli.EventEntry(nil), store...))
	if g := summaries(got); len(g) != 1 || g[0] != "d" {
		t.Fatalf("delivered = %v, want [d]", summaries(got))
	}
	csr.advance(got)
	if csr.watermark != 200 {
		t.Fatalf("watermark = %d, want 200", csr.watermark)
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
	csr := newSinceCursor()
	e := []cli.EventEntry{{Time: 300, UUID: "z", Summary: "z"}}
	csr.advance(e)
	csr.advance(e)
	csr.advance(e)
	if len(csr.sentAtWM) != 1 {
		t.Fatalf("sentAtWM grew to %d entries on repeated advance, want 1", len(csr.sentAtWM))
	}
	if !csr.containsWM("z") {
		t.Errorf("sentAtWM missing UUID after advance")
	}
}

// TestSinceCursor_Reset clears the watermark and dedup set on session swap.
func TestSinceCursor_Reset(t *testing.T) {
	csr := newSinceCursor()
	csr.advance(csr.filter([]cli.EventEntry{{Time: 500, UUID: "x", Summary: "x"}}))
	csr.reset()
	if csr.watermark != 0 {
		t.Fatalf("watermark after reset = %d, want 0", csr.watermark)
	}
	if csr.queryAfter() != -1 {
		t.Fatalf("queryAfter after reset = %d, want -1 (deliver everything)", csr.queryAfter())
	}
	if len(csr.sentAtWM) != 0 {
		t.Fatalf("dedup set not cleared on reset: %d entries", len(csr.sentAtWM))
	}
}
