package session

import (
	"testing"
	"time"
)

// TestCleanup_PruneSnapshot_RemovesCandidatesKeepsAlive verifies the
// R20260602190132-PERF-5 (#1607) refactor: the prune phase now re-verifies and
// deletes only the candidate keys snapshotted under RLock, while alive sessions
// survive and activeCount is set from the reconcile pass's alive total.
func TestCleanup_PruneSnapshot_RemovesCandidatesKeepsAlive(t *testing.T) {
	r := &Router{
		sessions:         make(map[string]*ManagedSession),
		backendOverrides: map[string]string{},
		maxProcs:         10,
		ttl:              1 * time.Minute,
		pruneTTL:         1 * time.Hour,
	}

	// nil-process stub past pruneTTL → prune candidate.
	stub := &ManagedSession{key: "stub"}
	stub.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	r.sessions["stub"] = stub
	r.backendOverrides["stub"] = "kiro"

	// dead process past pruneTTL with no session ID → prune candidate.
	deadSession := injectSession(r, "dead", newDeadProc())
	deadSession.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	// alive idle sessions within ttl → must survive and count as active.
	aliveA := injectSession(r, "aliveA", newIdleProc())
	aliveA.touchLastActive()
	aliveB := injectSession(r, "aliveB", newIdleProc())
	aliveB.touchLastActive()

	r.Cleanup()

	if _, ok := r.sessions["stub"]; ok {
		t.Error("nil-process stub past pruneTTL should be pruned")
	}
	if _, ok := r.backendOverrides["stub"]; ok {
		t.Error("pruned stub's backendOverride should be freed")
	}
	if _, ok := r.sessions["dead"]; ok {
		t.Error("dead session past pruneTTL should be pruned")
	}
	if _, ok := r.sessions["aliveA"]; !ok {
		t.Error("alive session aliveA should survive cleanup")
	}
	if _, ok := r.sessions["aliveB"]; !ok {
		t.Error("alive session aliveB should survive cleanup")
	}

	active, total := r.Stats()
	if total != 2 {
		t.Errorf("total sessions after prune = %d, want 2", total)
	}
	if active != 2 {
		t.Errorf("activeCount after prune = %d, want 2 (both alive sessions)", active)
	}
}

// TestCleanup_PruneSnapshot_ReVerifiesUnderLock confirms a candidate that is no
// longer prunable by the time the write lock is held is NOT removed. We emulate
// the "refreshed between snapshot and re-verify" race by making the session a
// candidate (old lastActive) but then touching it after Cleanup's RLock pass
// would not normally re-run — instead we assert shouldPrune is the single
// authority: a session whose lastActive is fresh is never pruned, exercising the
// !shouldPrune skip branch in the write-locked loop.
func TestCleanup_PruneSnapshot_ReVerifiesUnderLock(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 5,
		ttl:      1 * time.Minute,
		pruneTTL: 1 * time.Hour,
	}

	// Alive process but lastActive past pruneTTL: shouldPrune consults the
	// process and finds it Alive, so it must NOT be pruned even though it aged
	// past pruneTTL. This locks in that the re-verify under the write lock uses
	// the same shouldPrune authority and does not blindly delete aged keys.
	s := injectSession(r, "agedButAlive", newIdleProc())
	s.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	r.Cleanup()

	if _, ok := r.sessions["agedButAlive"]; !ok {
		t.Error("aged-but-alive session must not be pruned (alive process)")
	}
}
