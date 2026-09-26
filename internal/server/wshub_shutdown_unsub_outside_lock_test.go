package server

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestHubShutdown_UnsubInvokedOutsideHubMu: Shutdown runs every client's
// unsub closures after the registry's lock is released, so no per-key foreign
// mutex (eventLog.Unsubscribe / scheduler.Unsubscribe) is acquired under it.
// The closure here tries the registry's lock: if Shutdown ran it under that
// lock, TryLock fails (and a blocking Lock would deadlock Shutdown).
//
// We bound the test on a 2s deadline — well above goroutine-scheduling
// jitter, well below CI patience. The closure also asserts that the registry lock is
// observably free at invocation time so a future regression that
// accidentally re-acquired the registry lock before invoking unsubs would also fail.
func TestHubShutdown_UnsubInvokedOutsideHubMu(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	hub := NewHub(HubOptions{
		Router: router,
		Guard:  guard,
	})

	// Build a fake wsClient with a subscription map. The unsub closure
	// flips the flag once invoked; if it is ever called inside the registry lock the
	// the registry lock.TryLock() probe below will fail (RWMutex.TryLock is the cleanest
	// "is the lock currently free?" probe in the stdlib).
	var unsubInvokedOutsideLock atomic.Bool
	var unsubCalled atomic.Bool
	c := &wsClient{done: make(chan struct{})}
	registerSub(hub, c, "")
	subscribeTest(hub, c, "k1", func() {
		unsubCalled.Store(true)
		if hub.subs.mu.TryLock() {
			hub.subs.mu.Unlock()
			unsubInvokedOutsideLock.Store(true)
		}
	})

	done := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown deadlocked or hung past 2s — R249-PERF-24 (#939) regressed: per-client unsub closure called inside the registry lock, blocking on a foreign mutex held with the registry lock acquired")
	}

	if !unsubCalled.Load() {
		t.Fatal("unsub closure never invoked during Shutdown — Shutdown contract changed")
	}
	if !unsubInvokedOutsideLock.Load() {
		t.Error("unsub closure observed the registry lock held at invocation time — R249-PERF-24 (#939) regressed: Shutdown invoked unsubs while still holding the registry lock")
	}
}
