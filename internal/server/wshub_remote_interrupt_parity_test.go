package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// failInterruptNode embeds fakeCapNode but fails ProxyInterruptSession so the
// remote-interrupt error path runs. id matches the registered node key.
type failInterruptNode struct {
	fakeCapNode
}

func (f *failInterruptNode) ProxyInterruptSession(_ context.Context, _ string) (bool, error) {
	return false, errors.New("node offline")
}

// newTestHubWithNodes builds a Hub wired with a static node map so the
// remote-* handlers can resolve a peer without a real reverse connection.
func newTestHubWithNodes(nodes map[string]node.Conn) (*Hub, *sync.RWMutex) {
	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{
		Router:    router,
		Guard:     guard,
		Nodes:     nodes,
		NodesMu:   &nodesMu,
		DashToken: "tok",
		CookieMAC: testCookieMAC("tok"),
	})
	return hub, &nodesMu
}

// TestHandleRemoteInterrupt_FailureBroadcastsToSubscribers verifies R176-ARCH-NX
// (#433) parity: a failed remote interrupt fans a `system` event out to every
// dashboard subscribed to the session, not just the originating client's
// interrupt_ack.
func TestHandleRemoteInterrupt_FailureBroadcastsToSubscribers(t *testing.T) {
	const key = "node1:p2p:carol"
	nodes := map[string]node.Conn{"node1": &failInterruptNode{fakeCapNode{id: "node1"}}}
	hub, _ := newTestHubWithNodes(nodes)
	t.Cleanup(hub.Shutdown)

	// Originating client (issues the interrupt) and a separate watcher that
	// is subscribed to the same session key.
	origin, originOut := newCapturedClient(t, hub)
	watcher, watcherOut := newCapturedClient(t, hub)
	registerSub(hub, origin, key)
	registerSub(hub, watcher, key)

	hub.handleRemoteInterrupt(origin, node.ClientMsg{Node: "node1", Key: key, ID: "rid"})

	// Originating client gets the terse interrupt_ack error.
	sawAck := false
	sawWatcherEvent := false
	deadline := time.After(time.Second)
	for !(sawAck && sawWatcherEvent) {
		select {
		case m := <-originOut:
			if m.Type == "interrupt_ack" && m.Status == "error" {
				sawAck = true
			}
			if m.Type == "event" && m.Event != nil && m.Event.Type == "system" {
				// origin is also a subscriber, so it receives the broadcast too.
			}
		case m := <-watcherOut:
			if m.Type == "event" && m.Event != nil && m.Event.Type == "system" {
				if m.Key != key {
					t.Fatalf("event Key = %q, want %q", m.Key, key)
				}
				sawWatcherEvent = true
			}
		case <-deadline:
			t.Fatalf("timeout: sawAck=%v sawWatcherEvent=%v", sawAck, sawWatcherEvent)
		}
	}
}
