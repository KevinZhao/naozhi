package session

import (
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/metrics"
)

// asyncCloseProc models the real CLI/shim teardown where Close() does NOT
// synchronously flip Alive() — Alive() only goes false after a later channel
// transition. This is exactly the window #1645 describes: evictOldest's
// post-Close countActive() reconcile can still observe the evictee as alive.
type asyncCloseProc struct {
	mu        sync.Mutex
	alive     bool
	isRunning bool
}

func (p *asyncCloseProc) Alive() bool     { p.mu.Lock(); defer p.mu.Unlock(); return p.alive }
func (p *asyncCloseProc) IsRunning() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.isRunning }
func (p *asyncCloseProc) Close()          { /* async: Alive stays true until a later transition */ }

// asyncIdleProc embeds fakeProcess for the bulk of processIface but overrides
// the liveness trio with async-close semantics.
type asyncIdleProc struct {
	*fakeProcess
	async *asyncCloseProc
}

func newAsyncIdleProc() *asyncIdleProc {
	fp := &fakeProcess{isAlive: true, isRunning: false}
	return &asyncIdleProc{fakeProcess: fp, async: &asyncCloseProc{alive: true}}
}

func (p *asyncIdleProc) Alive() bool     { return p.async.Alive() }
func (p *asyncIdleProc) IsRunning() bool { return p.async.IsRunning() }
func (p *asyncIdleProc) Close()          { p.async.Close() }

// TestEvictOldest_GaugeMatchesReconcile pins #1645: after evictOldest the
// SessionActiveByBackend gauge must equal countActive()'s absolute recount.
// The fix removed the explicit RecordSessionActive(-1) that ran before the
// reconcile; this test guards the invariant that the gauge tracks the recount
// (and that the manual decrement, were it re-added, never drives the gauge
// below the true alive count). Uses a real ("claude") backend label so the
// assertion is not confused by the empty-label bucket aliasing of the default
// backend. Exercises the async-Close window the issue calls out: the evictee's
// Alive() stays true through Close(), so the recount still includes it.
func TestEvictOldest_GaugeMatchesReconcile(t *testing.T) {
	const backend = "claude"
	resetBackendGauge(t, backend)

	r := newTestRouter(2)

	// Two idle sessions on the same backend, both alive.
	older := injectSession(r, "dashboard:direct:user:a", newAsyncIdleProc())
	older.SetBackend(backend)
	older.lastActive.Store(1) // oldest → eviction target
	newer := injectSession(r, "dashboard:direct:user:b", newAsyncIdleProc())
	newer.SetBackend(backend)
	newer.lastActive.Store(2)

	// Seed the gauge to the truthful count (2).
	r.mu.Lock()
	r.reconcileSessionActiveByBackendLocked()
	r.mu.Unlock()
	if got := metrics.SessionActiveByBackend.Get(backend); got != 2 {
		t.Fatalf("precondition: gauge=%d, want 2", got)
	}

	// Evict. The async proc keeps Alive()==true through Close(), so the
	// reconcile still counts the evictee — final gauge must equal the recount
	// (2), NOT 1 (the buggy manual -1 baseline that would have driven the
	// pre-reconcile value off the true count).
	r.mu.Lock()
	evicted := r.evictOldest()
	gauge := metrics.SessionActiveByBackend.Get(backend)
	r.mu.Unlock()

	if !evicted {
		t.Fatal("evictOldest returned false, expected an eviction")
	}
	if gauge != 2 {
		t.Fatalf("gauge=%d after evict with async-alive evictee, want 2 (manual -1 must be gone)", gauge)
	}
}

func resetBackendGauge(t *testing.T, backend string) {
	t.Helper()
	if cur := metrics.SessionActiveByBackend.Get(backend); cur != 0 {
		metrics.SessionActiveByBackend.Add(-cur, backend)
	}
	if got := metrics.SessionActiveByBackend.Get(backend); got != 0 {
		t.Fatalf("failed to reset gauge for %q: %d", backend, got)
	}
}
