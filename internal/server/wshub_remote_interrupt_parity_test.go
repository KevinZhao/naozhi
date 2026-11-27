package server

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
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

// newTestHubWithNodes builds a Hub wired with a static node table so the
// remote-* handlers can resolve a peer without a real reverse connection.
func newTestHubWithNodes(t testing.TB, nodes map[string]node.Conn) *Hub {
	t.Helper()
	router := session.NewRouter(session.RouterConfig{})
	return newHubForTest(t, HubOptions{
		Router:    router,
		Nodes:     newNodeRegistry(nodes),
		DashToken: "tok",
		CookieMAC: testCookieMAC("tok"),
	}, sendEngineOpts{})
}

// TestHandleRemoteInterrupt_FailureBroadcastsToSubscribers verifies R176-ARCH-NX
// (#433) parity: a failed remote interrupt fans a `system` event out to every
// dashboard subscribed to the session, not just the originating client's
// interrupt_ack.
func TestHandleRemoteInterrupt_FailureBroadcastsToSubscribers(t *testing.T) {
	const key = "node1:p2p:carol"
	nodes := map[string]node.Conn{"node1": &failInterruptNode{fakeCapNode{id: "node1"}}}
	hub := newTestHubWithNodes(t, nodes)
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

// failSendNode fails Send so the remote-send error path runs.
type failSendNode struct {
	fakeCapNode
}

func (f *failSendNode) Send(context.Context, node.SendRequest) (node.SendResult, error) {
	return node.SendResult{}, errors.New("dial tcp 10.0.0.5:8180: connection refused")
}

// TestRemoteFailure_ReachesRemoteWatchers: a tab watching a remote session
// holds its subscription on the node conn, never in the hub's registry, so
// a failed remote send or interrupt must still reach it, naming the node,
// and without the raw transport error.
func TestRemoteFailure_ReachesRemoteWatchers(t *testing.T) {
	const key = "feishu:direct:alice:general"
	for _, tc := range []struct {
		name string
		conn node.Conn
		do   func(h *Hub, c *wsClient)
		want string
	}{
		{"send", &failSendNode{fakeCapNode{id: "node1"}}, func(h *Hub, c *wsClient) {
			h.handleRemoteSend(c, node.ClientMsg{Node: "node1", Key: key, Text: "hi", ID: "sid"})
		}, "发送失败：remote send failed"},
		{"interrupt", &failInterruptNode{fakeCapNode{id: "node1"}}, func(h *Hub, c *wsClient) {
			h.handleRemoteInterrupt(c, node.ClientMsg{Node: "node1", Key: key, ID: "rid"})
		}, "中断失败：remote interrupt failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newTestHubWithNodes(t, map[string]node.Conn{"node1": tc.conn})
			t.Cleanup(hub.Shutdown)
			origin, _ := newCapturedClient(t, hub)
			watcher, watcherOut := newCapturedClient(t, hub)
			// Both watch the remote session: registered, no local subscription.
			registerSub(hub, origin, "")
			registerSub(hub, watcher, "")

			tc.do(hub, origin)
			deadline := time.After(2 * time.Second)
			for {
				select {
				case m := <-watcherOut:
					if m.Type != "event" || m.Event == nil || m.Event.Type != "system" {
						continue
					}
					if m.Key != key || m.Node != "node1" || m.Event.Summary != tc.want {
						t.Fatalf("event = key %q node %q summary %q, want %q on node1 with %q", m.Key, m.Node, m.Event.Summary, key, tc.want)
					}
					return
				case <-deadline:
					t.Fatal("the remote watcher heard nothing of the failure")
				}
			}
		})
	}
}

// countSendNode counts the sends that reach the node.
type countSendNode struct {
	fakeCapNode
	sends atomic.Int32
}

func (f *countSendNode) Send(context.Context, node.SendRequest) (node.SendResult, error) {
	f.sends.Add(1)
	return node.SendResult{}, nil
}

// TestHandleSend_RemoteKeyValidated: a send naming a node gets the same key
// check as a local one before anything on this node logs or echoes the key.
func TestHandleSend_RemoteKeyValidated(t *testing.T) {
	for key, want := range map[string]string{
		"":                        "key is required",
		"dashboard:direct:\u202e": "invalid key",
		strings.Repeat("k", 8192): "invalid key",
	} {
		n := &countSendNode{fakeCapNode: fakeCapNode{id: "node1"}}
		hub := newTestHubWithNodes(t, map[string]node.Conn{"node1": n})
		c, out := newCapturedClient(t, hub)
		hub.handleSend(c, node.ClientMsg{Node: "node1", Key: key, Text: "hi", ID: "sid"})
		select {
		case m := <-out:
			if m.Type != "send_ack" || m.Status != "error" || m.Error != want || m.Key != "" {
				t.Errorf("key %.20q: ack = type %q status %q error %q key %.20q, want error %q with no key", key, m.Type, m.Status, m.Error, m.Key, want)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("key %.20q: no send_ack", key)
		}
		hub.Shutdown()
		if got := n.sends.Load(); got != 0 {
			t.Errorf("key %.20q: %d sends reached the node", key, got)
		}
	}
}
