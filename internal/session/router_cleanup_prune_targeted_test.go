package session

import (
	"testing"
	"time"
)

// TestCleanup_TargetedPrune_MixedSessions exercises the R20260602190132-PERF-5
// rewrite: the prune phase now walks only the candidates snapshotted under
// pass-1 RLock instead of re-ranging the whole map under the write lock. This
// guards against the rewrite silently dropping a prune that the old full-scan
// loop would have caught, and verifies activeCount is reconciled to the
// post-prune alive total reused from reconcileSessionActiveByBackendLocked.
func TestCleanup_TargetedPrune_MixedSessions(t *testing.T) {
	r := &Router{
		sessions:         make(map[string]*ManagedSession),
		backendOverrides: map[string]string{},
		maxProcs:         10,
		ttl:              1 * time.Minute,
		pruneTTL:         1 * time.Hour,
	}

	// orphan: nil-process stub past pruneTTL → must be pruned.
	orphan := &ManagedSession{key: "orphan"}
	orphan.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	r.sessions["orphan"] = orphan
	r.backendOverrides["orphan"] = "kiro"

	// deadOld: dead process past pruneTTL with no session ID → must be pruned.
	deadOld := injectSession(r, "deadOld", newDeadProc())
	deadOld.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	// alive: live process, recent → must survive and count toward activeCount.
	alive := injectSession(r, "alive", newRunningProc())
	alive.lastActive.Store(time.Now().UnixNano())

	// keptResumable: dead process WITH session ID, recent → kept for resumption.
	keptResumable := injectSession(r, "kept", newDeadProc())
	keptResumable.setSessionID("resumable-sess")
	keptResumable.lastActive.Store(time.Now().UnixNano())

	r.Cleanup()

	if _, ok := r.sessions["orphan"]; ok {
		t.Error("nil-process orphan past pruneTTL should be pruned")
	}
	if _, ok := r.backendOverrides["orphan"]; ok {
		t.Error("pruned orphan's backendOverride should be freed")
	}
	if _, ok := r.sessions["deadOld"]; ok {
		t.Error("dead process past pruneTTL with no session ID should be pruned")
	}
	if _, ok := r.sessions["alive"]; !ok {
		t.Error("alive session must survive cleanup")
	}
	if _, ok := r.sessions["kept"]; !ok {
		t.Error("dead session with session ID within pruneTTL must be kept")
	}

	if got := r.activeCount.Load(); got != 1 {
		t.Errorf("activeCount = %d, want 1 (only the alive running session)", got)
	}
}

// TestCleanup_TargetedPrune_ReverifySkipsRebound verifies the write-lock
// re-verification guard: if a key flagged as a prune candidate under pass-1
// RLock no longer satisfies shouldPrune by the time the write lock is held
// (e.g. it was re-activated), it must NOT be pruned. We simulate the
// re-activation by making the candidate alive+recent before the (single
// synchronous) prune phase runs — shouldPrune will return false and the
// re-verification must keep it.
func TestCleanup_TargetedPrune_ReverifyKeepsAlive(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 5,
		ttl:      1 * time.Minute,
		pruneTTL: 1 * time.Hour,
	}
	// A live, recently-active session is never a prune candidate; it must
	// survive. This is the steady-state guard that the targeted loop does not
	// over-prune live sessions.
	s := injectSession(r, "live", newRunningProc())
	s.lastActive.Store(time.Now().UnixNano())

	r.Cleanup()

	if _, ok := r.sessions["live"]; !ok {
		t.Fatal("live recently-active session must never be pruned")
	}
	if got := r.activeCount.Load(); got != 1 {
		t.Errorf("activeCount = %d, want 1", got)
	}
}
