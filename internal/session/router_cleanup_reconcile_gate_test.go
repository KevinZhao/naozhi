package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestCleanup_ReconcileGate_NoOpKeepsGaugeCorrect pins R20260602-PERF-1
// (#1627): a steady-state Cleanup tick with no close / prune and no change
// in the alive count must NOT corrupt the per-backend SessionActive gauge.
// The reconcile is now gated, so this verifies the gauge stays correct (the
// gate must never leave it stale) on the common no-op path.
func TestCleanup_ReconcileGate_NoOpKeepsGaugeCorrect(t *testing.T) {
	// Drive the labeled gauge to a known-empty baseline for the "" backend.
	metrics.SessionActiveByBackend.Add(-metrics.SessionActiveByBackend.Get(""), "")
	metrics.SessionActive.Add(-metrics.SessionActive.Value())

	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		maxProcs:     3,
		ttl:          1 * time.Hour, // long TTL so the idle session is NOT expired
		pruneTTL:     72 * time.Hour,
		totalTimeout: 5 * time.Minute,
	}
	// One live, idle (not running), recently-active session: survives the
	// tick untouched (no close, no prune, alive count unchanged).
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().UnixNano())

	// Seed the gauge to match the live set so reconcile would be a no-op.
	r.reconcileSessionActiveByBackendLocked()
	wantBackend := metrics.SessionActiveByBackend.Get(s.Backend())
	if wantBackend != 1 {
		t.Fatalf("precondition: gauge for backend %q = %d, want 1", s.Backend(), wantBackend)
	}

	r.Cleanup()

	if got := metrics.SessionActiveByBackend.Get(s.Backend()); got != 1 {
		t.Errorf("after no-op Cleanup: per-backend gauge = %d, want 1 (gate must not drop a still-alive session)", got)
	}
	if got := r.activeCount.Load(); got != 1 {
		t.Errorf("after no-op Cleanup: activeCount = %d, want 1", got)
	}
}

// TestCleanup_ReconcileGate_PruneDrivesGaugeToZero pins R20260602-PERF-1
// (#1627): when a session is actually pruned the gate must fire the
// reconcile so the per-backend gauge is driven down to the new live count.
func TestCleanup_ReconcileGate_PruneDrivesGaugeToZero(t *testing.T) {
	metrics.SessionActiveByBackend.Add(-metrics.SessionActiveByBackend.Get(""), "")
	metrics.SessionActive.Add(-metrics.SessionActive.Value())

	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     1 * time.Minute,
		totalTimeout: 5 * time.Minute,
	}
	// Dead process aged past pruneTTL → shouldPrune true → unregister.
	proc := newDeadProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	// Seed the gauge to a non-zero value so we can observe the drive-to-zero.
	metrics.SessionActiveByBackend.Add(1, s.Backend())

	r.Cleanup()

	if _, ok := r.sessions["key1"]; ok {
		t.Fatal("precondition: session should have been pruned")
	}
	if got := metrics.SessionActiveByBackend.Get(""); got != 0 {
		t.Errorf("after prune Cleanup: per-backend gauge = %d, want 0 (reconcile must run on prune)", got)
	}
}
