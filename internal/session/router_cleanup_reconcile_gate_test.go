package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestCleanup_ReconcileGate_NoOpKeepsGaugeCorrect: a steady-state Cleanup tick
// with no close / prune must keep the per-backend SessionActive gauge correct.
// The earlier #1627 reconcile-gate was superseded by the master PERF-5 (#1607)
// rewrite, which now runs reconcileSessionActiveByBackendLocked unconditionally
// to derive the authoritative alive total; this test pins that the
// unconditional reconcile leaves a still-alive session's gauge untouched.
func TestCleanup_ReconcileGate_NoOpKeepsGaugeCorrect(t *testing.T) {
	// Use a dedicated backend label so a concurrent test mutating the default
	// "" bucket of the global SessionActiveByBackend gauge cannot pollute this
	// assertion. reconcileSessionActiveByBackendLocked only touches the label
	// of sessions actually present, so an isolated label is fully isolated.
	const backend = "reconcile-gate-noop"
	resetBackendGauge(t, backend)

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
	s.SetBackend(backend)
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

// TestCleanup_ReconcileGate_PruneDrivesGaugeToZero: when a session is actually
// pruned, the unconditional reconcile (master PERF-5 #1607) drives the
// per-backend gauge down to the new live count.
func TestCleanup_ReconcileGate_PruneDrivesGaugeToZero(t *testing.T) {
	const backend = "reconcile-gate-prune"
	resetBackendGauge(t, backend)

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
	s.SetBackend(backend)
	s.lastActive.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	// Seed the gauge to a non-zero value so we can observe the drive-to-zero.
	metrics.SessionActiveByBackend.Add(1, s.Backend())

	r.Cleanup()

	if _, ok := r.sessions["key1"]; ok {
		t.Fatal("precondition: session should have been pruned")
	}
	if got := metrics.SessionActiveByBackend.Get(backend); got != 0 {
		t.Errorf("after prune Cleanup: per-backend gauge = %d, want 0 (reconcile must run on prune)", got)
	}
}
