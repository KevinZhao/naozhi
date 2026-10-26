package session

import (
	"testing"
	"time"
)

// TestCleanup_ReconcileGate_NoOpKeepsActiveCorrect: a steady-state Cleanup
// tick with no close / prune keeps a still-alive session in activeCount.
// R20260603-PERF-7: when closedCount==0 && pruned==0 the recount walk is
// skipped and activeCount is read from the atomic.
func TestCleanup_ReconcileGate_NoOpKeepsActiveCorrect(t *testing.T) {
	r := &Router{
		ss:       newSessionTable(),
		maxProcs: 3,
		ttl:      1 * time.Hour, // long TTL so the idle session is NOT expired
		pruneTTL: 72 * time.Hour,
		spawn:    spawnConfig{totalTimeout: 5 * time.Minute},
	}
	// One live, idle (not running), recently-active session: survives the
	// tick untouched (no close, no prune, alive count unchanged).
	s := injectSession(r, "key1", newIdleProc())
	s.lastActive.Store(time.Now().UnixNano())
	setActiveT(r, 1)

	r.Cleanup()

	if got := r.ss.Active(); got != 1 {
		t.Errorf("after no-op Cleanup: activeCount = %d, want 1", got)
	}
}

// TestCleanup_ReconcileGate_PruneRecountsActive: when a session is actually
// pruned, the recount (master PERF-5 #1607) drives activeCount down to the
// new live count.
func TestCleanup_ReconcileGate_PruneRecountsActive(t *testing.T) {
	r := &Router{
		ss:       newSessionTable(),
		maxProcs: 3,
		ttl:      1 * time.Minute,
		pruneTTL: 1 * time.Minute,
		spawn:    spawnConfig{totalTimeout: 5 * time.Minute},
	}
	// Dead process aged past pruneTTL → shouldPrune true → unregister.
	s := injectSession(r, "key1", newDeadProc())
	s.lastActive.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	setActiveT(r, 1)

	r.Cleanup()

	if _, ok := lookupT(r, "key1"); ok {
		t.Fatal("precondition: session should have been pruned")
	}
	if got := r.ss.Active(); got != 0 {
		t.Errorf("after prune Cleanup: activeCount = %d, want 0 (recount must run on prune)", got)
	}
}
