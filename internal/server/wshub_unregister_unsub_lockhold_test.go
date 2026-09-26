package server

import (
	"sync"
	"testing"
)

// TestUnregister_UnsubClosuresInvokedOutsideMu: unregister runs a client's
// unsub closures after releasing the registry's lock, so a heavy-tab
// disconnect (50 subs) does not hold the lock across 50 closure calls. The
// closure below asserts the lock is free; were it called under the lock,
// TryLock would fail.
func TestUnregister_UnsubClosuresInvokedOutsideMu(t *testing.T) {
	hub, _ := newTestHub("")
	t.Cleanup(hub.Shutdown)

	// A subscription whose unsub closure tries to take the registry's lock:
	// unregister must not hold it when the closure runs, or TryLock fails.
	c := &wsClient{done: make(chan struct{})}
	var (
		mu        sync.Mutex
		muHeld    bool
		invokeErr error
	)
	registerSub(hub, c, "")
	subscribeTest(hub, c, "k1", func() {
		// The registry's lock must be released by the time we get here.
		if !hub.subs.mu.TryLock() {
			mu.Lock()
			invokeErr = errLockStillHeld
			mu.Unlock()
			return
		}
		muHeld = true
		hub.subs.mu.Unlock()
	})

	hub.unregister(c)

	mu.Lock()
	defer mu.Unlock()
	if invokeErr != nil {
		t.Fatalf("unsub closure observed the registry lock still held: %v", invokeErr)
	}
	if !muHeld {
		t.Fatal("unsub closure did not run; check unregister snapshot path")
	}
}

// errLockStillHeld is the sentinel surfaced when the unsub closure cannot
// acquire the registry lock. Distinct error type so future test additions that reuse
// this pattern can errors.Is rather than string-match.
var errLockStillHeld = lockHeldErr("registry lock still held when unsub closure ran")

type lockHeldErr string

func (e lockHeldErr) Error() string { return string(e) }
