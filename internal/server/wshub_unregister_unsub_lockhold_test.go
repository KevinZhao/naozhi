package server

import (
	"sync"
	"testing"
)

// TestUnregister_UnsubClosuresInvokedOutsideMu pins R249-PERF-23 (#938):
// Hub.unregister now snapshots c.subscriptions under h.mu and invokes the
// per-key unsub closures *after* releasing h.mu so a heavy-tab disconnect
// (50 subs) does not serialise lock-hold over 50 closure invocations.
//
// We verify the contract by installing an unsub closure that itself
// asserts h.mu is acquirable — pre-fix this would deadlock (we'd reach
// the closure inside h.mu, the closure tries to take h.mu, AA on the same
// goroutine wedges since sync.RWMutex is not reentrant). Post-fix the
// closure runs after h.mu.Unlock so the recursive Lock succeeds.
func TestUnregister_UnsubClosuresInvokedOutsideMu(t *testing.T) {
	hub, _ := newTestHub("")
	t.Cleanup(hub.Shutdown)

	// Direct exercise: synthesise a wsClient with a subscription whose
	// unsub closure tries to take h.mu. unregister must not be holding
	// h.mu when the closure runs, otherwise the TryLock below fails.
	c := &wsClient{}
	c.subscriptions = make(map[string]func())
	var (
		mu        sync.Mutex
		muHeld    bool
		invokeErr error
	)
	c.subscriptions["k1"] = func() {
		// h.mu must be released by the time we get here.
		if !hub.mu.TryLock() {
			mu.Lock()
			invokeErr = errLockStillHeld
			mu.Unlock()
			return
		}
		muHeld = true
		hub.mu.Unlock()
	}

	// Insert into the hub so unregister's "if _, ok := h.clients[c]" branch
	// fires; otherwise the snapshot is empty and the closure never runs.
	hub.mu.Lock()
	if hub.clients == nil {
		hub.clients = map[*wsClient]struct{}{}
	}
	hub.clients[c] = struct{}{}
	hub.mu.Unlock()

	hub.unregister(c)

	mu.Lock()
	defer mu.Unlock()
	if invokeErr != nil {
		t.Fatalf("unsub closure observed h.mu still held: %v", invokeErr)
	}
	if !muHeld {
		t.Fatal("unsub closure did not run; check unregister snapshot path")
	}
}

// errLockStillHeld is the sentinel surfaced when the unsub closure cannot
// acquire h.mu. Distinct error type so future test additions that reuse
// this pattern can errors.Is rather than string-match.
var errLockStillHeld = lockHeldErr("h.mu still held when unsub closure ran")

type lockHeldErr string

func (e lockHeldErr) Error() string { return string(e) }
