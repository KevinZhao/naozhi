package session

import (
	"testing"
	"time"
)

// TestCleanup_IdleClosedSession_StaysOneTick pins the master PERF-5 (#1607)
// snapshot semantics: a session that is alive in the RLock pass-1 snapshot is
// NOT a prune candidate (shouldPrune consults the live process and returns
// false). When pass-2 then idle-closes it this tick, the closed session is
// intentionally left in the map for one cycle — it is only re-evaluated for
// prune on the NEXT Cleanup tick, once pass-1 observes the now-dead process.
//
// This replaced the earlier #1628 "closed-key fast path" tests: that
// optimization tried to prune an alive-then-closed session within the same
// tick, which contradicts the snapshot-and-reverify contract the PERF-5
// rewrite locked in (see TestCleanup_PruneSnapshot_ReVerifiesUnderLock).
func TestCleanup_IdleClosedSession_StaysOneTick(t *testing.T) {
	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     5 * time.Minute, // smaller than the idle age below
		totalTimeout: 5 * time.Minute,
	}
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	// Aged past both TTL (→ idle-closed this tick) and pruneTTL. Even so, the
	// session was alive at pass-1 snapshot, so it is not a pass-1 prune
	// candidate and must survive this tick.
	s.lastActive.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	r.Cleanup()

	if _, ok := r.sessions["key1"]; !ok {
		t.Fatal("alive-then-idle-closed session must survive the tick it is closed in (master PERF-5 snapshot semantics)")
	}
	if proc.Alive() {
		t.Error("idle session past TTL should have been closed this tick")
	}
	if got := r.activeCount.Load(); got != 0 {
		t.Errorf("activeCount = %d, want 0 (closed session must not count as alive)", got)
	}

	// Second tick: pass-1 now observes the dead process + stale lastActive, so
	// shouldPrune is true and the session is finally pruned.
	r.Cleanup()
	if _, ok := r.sessions["key1"]; ok {
		t.Fatal("dead session past pruneTTL must be pruned on the following tick")
	}
}
