package server

import (
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

// registerSub wires a captured client into the Hub maps and subscribes it to
// key. Mirrors the bookkeeping handleSubscribe / markAuthenticated do under
// h.mu so broadcastSessionSystemEvent's subscriber scan observes the client.
func registerSub(h *Hub, c *wsClient, key string) {
	h.mu.Lock()
	if h.clients == nil {
		h.clients = make(map[*wsClient]struct{})
	}
	if h.authClients == nil {
		h.authClients = make(map[*wsClient]struct{})
	}
	h.clients[c] = struct{}{}
	h.authClients[c] = struct{}{}
	if c.subscriptions == nil {
		c.subscriptions = make(map[string]func())
	}
	if key != "" {
		c.subscriptions[key] = func() {}
	}
	h.mu.Unlock()
}

func recvMsg(t *testing.T, out <-chan node.ServerMsg) (node.ServerMsg, bool) {
	t.Helper()
	select {
	case msg := <-out:
		return msg, true
	case <-time.After(time.Second):
		return node.ServerMsg{}, false
	}
}

// TestBroadcastSessionSystemEvent_ReachesSubscribers verifies R176-ARCH-NX
// (#433) parity: a remote-send failure fans out to every dashboard subscribed
// to the session key as a `system` event, not just the originating tab.
func TestBroadcastSessionSystemEvent_ReachesSubscribers(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	sub, subOut := newCapturedClient(t, hub)
	registerSub(hub, sub, "feishu:p2p:alice")

	hub.broadcastSessionSystemEvent("feishu:p2p:alice", "发送失败：remote down")

	msg, ok := recvMsg(t, subOut)
	if !ok {
		t.Fatal("subscriber received no frame")
	}
	if msg.Type != "event" {
		t.Fatalf("Type = %q, want event", msg.Type)
	}
	if msg.Key != "feishu:p2p:alice" {
		t.Fatalf("Key = %q, want feishu:p2p:alice", msg.Key)
	}
	if msg.Event == nil {
		t.Fatal("Event is nil")
	}
	if msg.Event.Type != "system" {
		t.Fatalf("Event.Type = %q, want system", msg.Event.Type)
	}
	if msg.Event.Summary != "发送失败：remote down" {
		t.Fatalf("Event.Summary = %q", msg.Event.Summary)
	}
	if msg.Event.Time == 0 {
		t.Fatal("Event.Time should be stamped")
	}
}

// TestBroadcastSessionSystemEvent_SkipsNonSubscribers ensures the failure is
// scoped to the session's subscribers — a tab watching a different session
// must not receive cross-tenant noise.
func TestBroadcastSessionSystemEvent_SkipsNonSubscribers(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	other, otherOut := newCapturedClient(t, hub)
	registerSub(hub, other, "feishu:p2p:bob")

	hub.broadcastSessionSystemEvent("feishu:p2p:alice", "发送失败：remote down")

	if _, ok := recvMsg(t, otherOut); ok {
		t.Fatal("non-subscriber should not receive the system event")
	}
}

// TestBroadcastSessionSystemEvent_NoSubscribersNoop verifies the
// snapshot-before-marshal fast path: a failure on a session nobody is watching
// must not deliver anything (and must not panic). The single connected client
// is subscribed to a DIFFERENT key, so the target key has zero subscribers.
func TestBroadcastSessionSystemEvent_NoSubscribersNoop(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	c, out := newCapturedClient(t, hub)
	registerSub(hub, c, "feishu:p2p:elsewhere")

	hub.broadcastSessionSystemEvent("node1:p2p:unwatched", "发送失败：x")

	if _, ok := recvMsg(t, out); ok {
		t.Fatal("a session with no subscribers should deliver nothing")
	}
}

// TestBroadcastSessionSystemEvent_MultipleSubscribers verifies every client
// subscribed to the key receives the system event after the #1902 two-phase
// snapshot (authMu membership snapshot → short h.mu.RLock subscription filter).
func TestBroadcastSessionSystemEvent_MultipleSubscribers(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const key = "feishu:p2p:alice"
	outs := make([]<-chan node.ServerMsg, 0, 3)
	for i := 0; i < 3; i++ {
		c, out := newCapturedClient(t, hub)
		registerSub(hub, c, key)
		outs = append(outs, out)
	}
	// A subscriber on a different key must stay silent.
	other, otherOut := newCapturedClient(t, hub)
	registerSub(hub, other, "feishu:p2p:bob")

	hub.broadcastSessionSystemEvent(key, "发送失败：remote down")

	for i, out := range outs {
		msg, ok := recvMsg(t, out)
		if !ok {
			t.Fatalf("subscriber %d received no frame", i)
		}
		if msg.Type != "event" || msg.Key != key || msg.Event == nil || msg.Event.Type != "system" {
			t.Fatalf("subscriber %d got unexpected frame: %+v", i, msg)
		}
	}
	if _, ok := recvMsg(t, otherOut); ok {
		t.Fatal("subscriber on a different key must not receive the event")
	}
}

// TestBroadcastSessionSystemEvent_ConcurrentChurn exercises the #1902 lock
// split under the race detector: while a goroutine continuously broadcasts
// session system events (taking authMu.RLock then h.mu.RLock), other
// goroutines churn the authClients / subscriptions maps the way register /
// unregister / handleSubscribe do under h.mu (+nested authMu). A client
// dropped from authClients between the two phases must not corrupt state or
// trip the detector. The test asserts no panic / no race; delivery counts are
// nondeterministic by design so they are not checked.
func TestBroadcastSessionSystemEvent_ConcurrentChurn(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const key = "feishu:p2p:churn"
	done := make(chan struct{})
	var wg sync.WaitGroup

	// drain continuously empties a captured client's output so SendRaw never
	// overflows the 64-deep buffer (which would trip the test helper's
	// non-idempotent done-close on the slow-client path).
	drain := func(out <-chan node.ServerMsg) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-out:
				case <-done:
					return
				}
			}
		}()
	}

	// Seed a stable subscriber so there is always something to deliver to.
	stable, stableOut := newCapturedClient(t, hub)
	registerSub(hub, stable, key)
	drain(stableOut)

	churn := make([]*wsClient, 8)
	for i := range churn {
		c, out := newCapturedClient(t, hub)
		registerSub(hub, c, key)
		drain(out)
		churn[i] = c
	}

	// Broadcaster: hammer the two-phase fan-out.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				hub.broadcastSessionSystemEvent(key, "发送失败：x")
			}
		}
	}()

	// Churner: add/remove clients from authClients + subscriptions the way the
	// real register/unregister writers do — h.mu held, authMu nested inside.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				for _, c := range churn {
					h := hub
					h.mu.Lock()
					h.authMu.Lock()
					delete(h.authClients, c)
					h.authMu.Unlock()
					delete(c.subscriptions, key)
					h.mu.Unlock()

					h.mu.Lock()
					h.authMu.Lock()
					h.authClients[c] = struct{}{}
					h.authMu.Unlock()
					c.subscriptions[key] = func() {}
					h.mu.Unlock()
				}
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}

// TestBroadcastSessionSystemEvent_ChunkedFilter verifies the #1925 chunked
// phase-2 subscription filter still delivers to every subscriber and skips
// non-subscribers when the candidate set spans more than one subFilterChunk.
// With > subFilterChunk authenticated clients the filter loop releases and
// re-acquires h.mu.RLock between batches; this asserts the chunk boundary math
// is correct (no client dropped or double-counted) across the seam.
func TestBroadcastSessionSystemEvent_ChunkedFilter(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const key = "feishu:p2p:chunk"
	// Span two full chunks plus a partial third so both the interior seam and
	// the final short batch are exercised.
	const subCount = subFilterChunk*2 + 5

	outs := make([]<-chan node.ServerMsg, 0, subCount)
	for i := 0; i < subCount; i++ {
		c, out := newCapturedClient(t, hub)
		registerSub(hub, c, key)
		outs = append(outs, out)
	}
	// Interleave non-subscribers (different key) so the filter must correctly
	// reject them across chunk boundaries too.
	const otherCount = subFilterChunk + 3
	otherOuts := make([]<-chan node.ServerMsg, 0, otherCount)
	for i := 0; i < otherCount; i++ {
		c, out := newCapturedClient(t, hub)
		registerSub(hub, c, "feishu:p2p:other")
		otherOuts = append(otherOuts, out)
	}

	hub.broadcastSessionSystemEvent(key, "发送失败：remote down")

	for i, out := range outs {
		msg, ok := recvMsg(t, out)
		if !ok {
			t.Fatalf("subscriber %d received no frame", i)
		}
		if msg.Type != "event" || msg.Key != key || msg.Event == nil || msg.Event.Type != "system" {
			t.Fatalf("subscriber %d got unexpected frame: %+v", i, msg)
		}
	}
	for i, out := range otherOuts {
		if _, ok := recvMsg(t, out); ok {
			t.Fatalf("non-subscriber %d must not receive the event", i)
		}
	}
}

// TestBroadcastSessionSystemEvent_EmptyArgsNoop guards the early return so an
// empty key or summary cannot emit a malformed frame.
func TestBroadcastSessionSystemEvent_EmptyArgsNoop(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	sub, subOut := newCapturedClient(t, hub)
	registerSub(hub, sub, "feishu:p2p:alice")

	hub.broadcastSessionSystemEvent("", "发送失败：x")
	hub.broadcastSessionSystemEvent("feishu:p2p:alice", "")

	if _, ok := recvMsg(t, subOut); ok {
		t.Fatal("empty key/summary should emit nothing")
	}
}
