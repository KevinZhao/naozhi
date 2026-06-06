package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestCleanup_ReconcileGate_NoOpKeepsGaugeCorrect: a steady-state Cleanup tick
// with no close / prune must keep the per-backend SessionActive gauge correct.
// R20260603-PERF-7: when closedCount==0 && pruned==0 the reconcile walk is
// skipped and activeCount is read from the atomic; this test pins that the
// skip path preserves a still-alive session's gauge and activeCount.
func TestCleanup_ReconcileGate_NoOpKeepsGaugeCorrect(t *testing.T) {
	// Use a dedicated backend label so a concurrent test mutating the default
	// "" bucket of the global SessionActiveByBackend gauge cannot pollute this
	// assertion. reconcileSessionActiveByBackendLocked only touches the label
	// of sessions actually present, so an isolated label is fully isolated.
	const backend = "reconcile-gate-noop"
	resetBackendGauge(t, backend)

	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
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
	if got := r.ss.activeCount.Load(); got != 1 {
		t.Errorf("after no-op Cleanup: activeCount = %d, want 1", got)
	}
}

// TestCleanup_ReconcileGate_NoOpSkipsReconcile: when closedCount==0 &&
// pruned==0 the Cleanup tick must skip the O(N) reconcile walk and preserve
// the existing activeCount (R20260603-PERF-7). We seed activeCount to a known
// value before the tick and assert it is unchanged afterwards.
func TestCleanup_ReconcileGate_NoOpSkipsReconcile(t *testing.T) {
	const backend = "reconcile-gate-skip"
	resetBackendGauge(t, backend)

	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:     3,
		ttl:          1 * time.Hour,
		pruneTTL:     72 * time.Hour,
		totalTimeout: 5 * time.Minute,
	}
	proc := newIdleProc()
	s := injectSession(r, "key-skip", proc)
	s.SetBackend(backend)
	s.lastActive.Store(time.Now().UnixNano())

	// Pre-seed activeCount and the gauge to the correct live count so a real
	// reconcile would be a no-op anyway — but we want to confirm the skip path
	// reaches the Store(aliveTotal) with the same value.
	r.ss.activeCount.Store(1)
	metrics.SessionActiveByBackend.Add(1, backend)

	r.Cleanup()

	// activeCount must remain 1 (preserved from atomic, not reset by reconcile).
	if got := r.ss.activeCount.Load(); got != 1 {
		t.Errorf("no-op Cleanup: activeCount=%d want 1 (PERF-7 skip-path must preserve count)", got)
	}
}

// TestCleanup_ReconcileGate_PruneDrivesGaugeToZero: when a session is actually
// pruned, the unconditional reconcile (master PERF-5 #1607) drives the
// per-backend gauge down to the new live count.
func TestCleanup_ReconcileGate_PruneDrivesGaugeToZero(t *testing.T) {
	const backend = "reconcile-gate-prune"
	resetBackendGauge(t, backend)

	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
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

	if _, ok := r.ss.sessions["key1"]; ok {
		t.Fatal("precondition: session should have been pruned")
	}
	if got := metrics.SessionActiveByBackend.Get(backend); got != 0 {
		t.Errorf("after prune Cleanup: per-backend gauge = %d, want 0 (reconcile must run on prune)", got)
	}
}
