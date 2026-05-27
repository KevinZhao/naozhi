package session

import (
	"strconv"
	"testing"
	"time"
)

// TestGuard_LastWaitOpportunisticPrune locks in the R217-GO-1 (#1306) fix:
// ShouldSendWait paths that never call Release used to grow lastWait
// unbounded. After the fix the map opportunistically drops stale entries
// once the size crosses lastWaitPruneThreshold so a sustained busy-key
// stream cannot leak the dedupe table.
func TestGuard_LastWaitOpportunisticPrune(t *testing.T) {
	t.Parallel()
	g := NewGuard()

	// Seed past lastWaitPruneThreshold entries, all backdated past the
	// stale horizon so the next ShouldSendWait should evict them.
	staleTS := time.Now().Add(-2 * lastWaitStale)
	g.waitMu.Lock()
	for i := 0; i < lastWaitPruneThreshold+10; i++ {
		g.lastWait["stale-"+strconv.Itoa(i)] = staleTS
	}
	g.waitMu.Unlock()

	// One ShouldSendWait on a fresh key: cap exceeded → opportunistic
	// sweep drops the stale entries and the fresh key is recorded.
	if !g.ShouldSendWait("fresh") {
		t.Fatal("ShouldSendWait should return true for an unseen key")
	}

	g.waitMu.Lock()
	size := len(g.lastWait)
	_, freshOK := g.lastWait["fresh"]
	g.waitMu.Unlock()

	if !freshOK {
		t.Fatal("fresh key was not recorded after sweep")
	}
	if size > 5 { // headroom for any concurrent test noise; we want "much smaller than threshold"
		t.Fatalf("lastWait size = %d after sweep, want ≤ 5 (only the fresh key + tolerance)", size)
	}
}

// TestGuard_LastWaitNoSweepBelowThreshold confirms small maps don't pay
// the prune walk: the implementation only runs the sweep once size hits
// lastWaitPruneThreshold so the common one-or-two-busy-key case stays fast.
func TestGuard_LastWaitNoSweepBelowThreshold(t *testing.T) {
	t.Parallel()
	g := NewGuard()

	// Seed a handful of stale entries — well under threshold.
	staleTS := time.Now().Add(-2 * lastWaitStale)
	g.waitMu.Lock()
	for i := 0; i < 10; i++ {
		g.lastWait["stale-"+strconv.Itoa(i)] = staleTS
	}
	g.waitMu.Unlock()

	if !g.ShouldSendWait("fresh") {
		t.Fatal("ShouldSendWait should succeed on unseen key")
	}

	g.waitMu.Lock()
	size := len(g.lastWait)
	g.waitMu.Unlock()
	// 10 stale + 1 fresh; sweep MUST NOT run because we're under threshold.
	if size != 11 {
		t.Fatalf("lastWait size = %d, want 11 (sweep should not run below threshold)", size)
	}
}
