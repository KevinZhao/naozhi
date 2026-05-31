package server

import (
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
