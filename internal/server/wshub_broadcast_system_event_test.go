package server

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestBroadcastSessionSystemEvent_UsesAuthMu pins R20260603150052-PERF-6:
// when authClients is initialised (production path via NewHub) the function
// must hold authMu, NOT the Hub-wide h.mu, while iterating. The test verifies
// this by asserting that a subscribed authenticated client receives the event
// even though h.mu is held exclusively by the caller at the time of the call
// — i.e. authMu is taken independently.
func TestBroadcastSessionSystemEvent_UsesAuthMu(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:alice:general"

	// Build a hub with authClients initialised (mirrors production NewHub path).
	h := &Hub{
		clients:     make(map[*wsClient]struct{}),
		authClients: make(map[*wsClient]struct{}),
	}

	var received atomic.Int32
	send := make(chan []byte, 4)
	done := make(chan struct{})
	c := &wsClient{
		hub:  h,
		send: send,
		done: done,
	}
	c.authenticated.Store(true)
	c.subscriptions = map[string]func(){key: nil}

	h.clients[c] = struct{}{}
	h.authClients[c] = struct{}{}

	// Hold h.mu exclusively for the duration of broadcastSessionSystemEvent.
	// If the implementation still takes h.mu.RLock inside the authClients branch
	// this will deadlock and the test times out (caught by -timeout).
	// The correct fix (authMu.RLock) must NOT try to acquire h.mu at all.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.mu.Lock()
		defer h.mu.Unlock()
		h.broadcastSessionSystemEvent(key, "test-summary")
	}()
	wg.Wait()

	// Drain the send channel and count deliveries.
	close(send)
	for range send {
		received.Add(1)
	}
	if received.Load() != 1 {
		t.Errorf("received %d events on subscribed client, want 1", received.Load())
	}
}

// TestBroadcastSessionSystemEvent_EmptyKeyOrSummaryNoOp guards the early-return
// guard: calls with blank key or summary must not panic and deliver nothing.
func TestBroadcastSessionSystemEvent_EmptyKeyOrSummaryNoOp(t *testing.T) {
	t.Parallel()
	h := &Hub{
		clients:     make(map[*wsClient]struct{}),
		authClients: make(map[*wsClient]struct{}),
	}
	// Must not panic.
	h.broadcastSessionSystemEvent("", "summary")
	h.broadcastSessionSystemEvent("key", "")
	h.broadcastSessionSystemEvent("", "")
}

// TestBroadcastSessionSystemEvent_LegacyFallback verifies that when authClients
// is nil (hand-rolled hub without NewHub) the function falls back to h.mu +
// h.clients scan and still delivers to subscribed authenticated clients.
func TestBroadcastSessionSystemEvent_LegacyFallback(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:bob:general"

	h := &Hub{
		clients: make(map[*wsClient]struct{}),
		// authClients intentionally nil — legacy path
	}

	send := make(chan []byte, 4)
	done := make(chan struct{})
	c := &wsClient{
		hub:  h,
		send: send,
		done: done,
	}
	c.authenticated.Store(true)
	c.subscriptions = map[string]func(){key: nil}
	h.clients[c] = struct{}{}

	h.broadcastSessionSystemEvent(key, "fallback-summary")

	close(send)
	var n int
	for range send {
		n++
	}
	if n != 1 {
		t.Errorf("legacy fallback delivered %d events, want 1", n)
	}
}
