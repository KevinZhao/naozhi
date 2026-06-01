package cron

import (
	"sync"
	"testing"
)

// TestCachedTZLabelStableForSameOffset verifies the memoised timezone label
// returns the exact string formatTZOffset would produce, and that repeated
// calls with the same (locName, offset) yield an identical, stable label —
// the property HandleList relies on so its 1 Hz poll does not re-run
// fmt.Sprintf. R103901-PERF-10.
func TestCachedTZLabelStableForSameOffset(t *testing.T) {
	h := &Handlers{}

	want := formatTZOffset("Asia/Shanghai", 8*3600)
	first := h.cachedTZLabel("Asia/Shanghai", 8*3600)
	if first != want {
		t.Fatalf("cachedTZLabel = %q, want %q", first, want)
	}
	for i := 0; i < 5; i++ {
		got := h.cachedTZLabel("Asia/Shanghai", 8*3600)
		if got != first {
			t.Fatalf("call %d returned %q, want stable %q", i, got, first)
		}
	}
}

// TestCachedTZLabelRecomputesOnOffsetChange guards the DST-safety invariant:
// a fixed *time.Location can still report different offsets across a DST
// transition, so the cache must key on the offset and recompute when it
// changes rather than freezing the first-seen label. R103901-PERF-10.
func TestCachedTZLabelRecomputesOnOffsetChange(t *testing.T) {
	h := &Handlers{}

	// Same location string, two offsets (e.g. EST -5h vs EDT -4h).
	est := h.cachedTZLabel("America/New_York", -5*3600)
	edt := h.cachedTZLabel("America/New_York", -4*3600)

	if est == edt {
		t.Fatalf("label did not change across offset flip: both %q", est)
	}
	if want := formatTZOffset("America/New_York", -5*3600); est != want {
		t.Fatalf("EST label = %q, want %q", est, want)
	}
	if want := formatTZOffset("America/New_York", -4*3600); edt != want {
		t.Fatalf("EDT label = %q, want %q", edt, want)
	}
	// Flipping back recomputes correctly (offset is the key, not insertion order).
	if back := h.cachedTZLabel("America/New_York", -5*3600); back != est {
		t.Fatalf("re-querying EST offset = %q, want %q", back, est)
	}
}

// TestCachedTZLabelConcurrent exercises the mutex under parallel callers,
// mirroring multiple dashboard tabs hitting HandleList at once. Run with -race.
func TestCachedTZLabelConcurrent(t *testing.T) {
	h := &Handlers{}
	want := formatTZOffset("UTC", 0)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := h.cachedTZLabel("UTC", 0); got != want {
				t.Errorf("concurrent cachedTZLabel = %q, want %q", got, want)
			}
		}()
	}
	wg.Wait()
}
