package server

import "testing"

// TestUnregister_UnsubClosuresInvokedOutsideMu: unregister runs a client's
// unsub closures after releasing the registry's lock, so a heavy-tab
// disconnect (50 subs) does not hold the lock across 50 closure calls. The
// closure below asserts the lock is free; were it called under the lock,
// TryLock would fail.
func TestUnregister_UnsubClosuresInvokedOutsideMu(t *testing.T) {
	hub, _ := newTestHub(t, "")
	t.Cleanup(hub.Shutdown)

	c := &wsClient{done: make(chan struct{})}
	var lockFree, ran bool
	registerSub(hub, c, "")
	subscribeTest(hub, c, "k1", func() {
		ran = true
		if hub.subs.mu.TryLock() {
			lockFree = true
			hub.subs.mu.Unlock()
		}
	})

	hub.unregister(c)

	if !ran {
		t.Fatal("unsub closure did not run; check unregister snapshot path")
	}
	if !lockFree {
		t.Fatal("unsub closure observed the registry lock still held")
	}
}
