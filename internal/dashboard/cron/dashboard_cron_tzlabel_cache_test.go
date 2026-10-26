package cron

import "testing"

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
