package session

import (
	"sync"
	"testing"
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

// TestEvictOldest_ActiveMatchesRecount pins #1645: after evictOldest the
// active count equals countActive()'s absolute recount — no manual -1 on top.
// Exercises the async-Close window the issue calls out: the evictee's Alive()
// stays true through Close(), so the recount still includes it.
func TestEvictOldest_ActiveMatchesRecount(t *testing.T) {
	r := newTestRouter(2)

	// Two idle sessions, both alive.
	older := injectSession(r, "dashboard:direct:user:a", newAsyncIdleProc())
	older.lastActive.Store(1) // oldest → eviction target
	newer := injectSession(r, "dashboard:direct:user:b", newAsyncIdleProc())
	newer.lastActive.Store(2)
	setActiveT(r, 2)

	var (
		evicted bool
		active  int64
	)
	r.ss.Update(func(tx sessTx) {
		evicted = r.evictOldest(tx)
		active = tx.Active()
	})

	if !evicted {
		t.Fatal("evictOldest returned false, expected an eviction")
	}
	if active != 2 {
		t.Fatalf("active=%d after evict with async-alive evictee, want 2 (manual -1 must be gone)", active)
	}
}
