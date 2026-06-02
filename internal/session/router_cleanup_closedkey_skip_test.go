package session

import (
	"testing"
	"time"
)

// TestCleanup_ClosedKey_NotPrunedWhenRecent pins R20260602-PERF-4 (#1628):
// a session expired (closed) this tick whose LastActive is still within
// pruneTTL must NOT be pruned by the closed-key fast path — it stays in the
// map as a dead-but-recent entry, exactly as the full shouldPrune path would
// decide. Closing the proc alone is not grounds for prune.
func TestCleanup_ClosedKey_NotPrunedWhenRecent(t *testing.T) {
	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     72 * time.Hour, // far larger than the idle age below
		totalTimeout: 5 * time.Minute,
	}
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	// Aged past TTL (→ expired/closed this tick) but well within pruneTTL.
	s.lastActive.Store(time.Now().Add(-25 * time.Minute).UnixNano())

	r.Cleanup()

	if _, ok := r.sessions["key1"]; !ok {
		t.Fatal("closed-but-recent session was wrongly pruned by the closed-key fast path; pruneTTL not yet reached")
	}
	if proc.Alive() {
		t.Error("idle session past TTL should have been closed this tick")
	}
	if got := r.activeCount.Load(); got != 0 {
		t.Errorf("activeCount = %d, want 0 (closed session must not count as alive)", got)
	}
}

// TestCleanup_ClosedKey_PrunedWhenStale pins the other half of #1628: when a
// closed session is ALSO past pruneTTL, the closed-key fast path must still
// prune it (same outcome as shouldPrune), keeping the optimization
// behaviour-preserving.
func TestCleanup_ClosedKey_PrunedWhenStale(t *testing.T) {
	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     5 * time.Minute, // smaller than the idle age below
		totalTimeout: 5 * time.Minute,
	}
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	// Aged past both TTL (→ closed this tick) and pruneTTL (→ pruned).
	s.lastActive.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	r.Cleanup()

	if _, ok := r.sessions["key1"]; ok {
		t.Fatal("closed session past pruneTTL must be pruned via the closed-key fast path")
	}
}
