package session

import (
	"testing"
	"time"
)

// TestUptimeString_CachesWithinSecondBucket locks down R65-PERF-L-1: two
// calls within the same second return the same string and share the same
// underlying snapshot pointer (no re-format).
func TestUptimeString_CachesWithinSecondBucket(t *testing.T) {
	// Start 5 seconds ago so rounding lands on a stable integer bucket.
	h := &Handlers{startedAt: time.Now().Add(-5 * time.Second)}

	first := h.uptimeStringAt(time.Now())
	snap1 := h.uptimeCache.Load()
	if snap1 == nil {
		t.Fatal("uptimeCache not populated after first call")
	}
	second := h.uptimeStringAt(time.Now())
	snap2 := h.uptimeCache.Load()

	if first != second {
		t.Errorf("uptimeString within same bucket returned different values: %q vs %q", first, second)
	}
	if snap1 != snap2 {
		t.Errorf("expected cached snapshot pointer to be reused within the same bucket")
	}
	if first == "" {
		t.Error("expected non-empty uptime string")
	}
}

// TestUptimeString_RotatesAcrossBuckets confirms the cache invalidates once
// the integer-second bucket advances (startedAt pushed backwards simulates
// the passage of time).
func TestUptimeString_RotatesAcrossBuckets(t *testing.T) {
	h := &Handlers{startedAt: time.Now().Add(-1 * time.Second)}
	first := h.uptimeStringAt(time.Now())
	// Shift startedAt back so the bucket id increases by at least one second.
	h.startedAt = h.startedAt.Add(-2 * time.Second)
	second := h.uptimeStringAt(time.Now())
	if first == second {
		t.Errorf("expected uptime to advance after bucket rotation, both = %q", first)
	}
}

