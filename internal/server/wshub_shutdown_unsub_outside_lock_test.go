package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestHubShutdown_UnsubInvokedOutsideHubMu pins R249-PERF-24 (#939):
// Hub.Shutdown must snapshot every client's subscription unsub closures
// while holding h.mu and invoke them AFTER releasing h.mu. The previous
// shape walked h.clients inside h.mu and called each unsub() inline —
// every per-key foreign mutex (eventLog.Unsubscribe / scheduler.Unsubscribe)
// was thus acquired under the Hub-wide lock. Mirror of the unregister
// fix landed for R249-PERF-23 (#938).
//
// Test strategy: register a fake wsClient holding a subscription whose
// unsub closure tries to acquire h.mu. Under the BUGGY ordering this
// deadlocks Shutdown forever (closure waits for h.mu held by Shutdown
// itself); under the FIXED ordering Shutdown drains h.mu before the
// closure runs and the closure observes h.mu free.
//
// We bound the test on a 2s deadline — well above goroutine-scheduling
// jitter, well below CI patience. The closure also asserts that h.mu is
// observably free at invocation time so a future regression that
// accidentally re-acquired h.mu before invoking unsubs would also fail.
func TestHubShutdown_UnsubInvokedOutsideHubMu(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{
		Router:  router,
		Guard:   guard,
		NodesMu: &nodesMu,
	})

	// Build a fake wsClient with a subscription map. The unsub closure
	// flips the flag once invoked; if it is ever called inside h.mu the
	// h.mu.TryLock() probe below will fail (RWMutex.TryLock is the cleanest
	// "is the lock currently free?" probe in the stdlib).
	var unsubInvokedOutsideLock atomic.Bool
	var unsubCalled atomic.Bool
	c := &wsClient{
		subscriptions: map[string]func(){
			"k1": func() {
				unsubCalled.Store(true)
				if hub.mu.TryLock() {
					hub.mu.Unlock()
					unsubInvokedOutsideLock.Store(true)
				}
			},
		},
	}

	hub.mu.Lock()
	hub.clients[c] = struct{}{}
	hub.subscriberCount["k1"] = 1
	hub.mu.Unlock()

	done := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown deadlocked or hung past 2s — R249-PERF-24 (#939) regressed: per-client unsub closure called inside h.mu, blocking on a foreign mutex held with h.mu acquired")
	}

	if !unsubCalled.Load() {
		t.Fatal("unsub closure never invoked during Shutdown — Shutdown contract changed")
	}
	if !unsubInvokedOutsideLock.Load() {
		t.Error("unsub closure observed h.mu held at invocation time — R249-PERF-24 (#939) regressed: Shutdown invoked unsubs while still holding h.mu")
	}
}
