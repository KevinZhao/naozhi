package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// wsUpgrader is used by tests that don't need origin checks.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
}

// ─── mock remote WS ─────────────────────────────────────────────────────────

// mockRemoteWS simulates a remote naozhi WS + HTTP endpoint.
type mockRemoteWS struct {
	token   string
	mu      sync.Mutex
	conns   []*mockWSConn
	subKeys map[string]struct{}
	handler http.HandlerFunc

	apiEvents []cli.EventEntry
}

// mockWSConn wraps a websocket.Conn with a write mutex to avoid races.
type mockWSConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *mockWSConn) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteJSON(v)
}

func newMockRemoteWS(token string) *mockRemoteWS {
	m := &mockRemoteWS{
		token:   token,
		subKeys: make(map[string]struct{}),
	}
	m.handler = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ws":
			m.handleWS(w, r)
		case r.URL.Path == "/api/sessions":
			json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{}})
		case r.URL.Path == "/api/sessions/events":
			json.NewEncoder(w).Encode(m.apiEvents)
		case r.URL.Path == "/api/sessions/send":
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
		case r.URL.Path == "/health":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.Error(w, "not found", 404)
		}
	}
	return m
}

func (m *mockRemoteWS) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	mc := &mockWSConn{conn: conn}
	m.mu.Lock()
	m.conns = append(m.conns, mc)
	m.mu.Unlock()
	defer conn.Close()

	authenticated := m.token == ""

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg node.ClientMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "auth":
			if m.token == "" || msg.Token == m.token {
				authenticated = true
				mc.writeJSON(node.ServerMsg{Type: "auth_ok"})
			} else {
				mc.writeJSON(node.ServerMsg{Type: "auth_fail", Error: "bad token"})
			}
		case "subscribe":
			if !authenticated {
				continue
			}
			m.mu.Lock()
			m.subKeys[msg.Key] = struct{}{}
			m.mu.Unlock()
			mc.writeJSON(node.ServerMsg{Type: "subscribed", Key: msg.Key, State: "ready"})
		case "unsubscribe":
			m.mu.Lock()
			delete(m.subKeys, msg.Key)
			m.mu.Unlock()
			mc.writeJSON(node.ServerMsg{Type: "unsubscribed", Key: msg.Key})
		}
	}
}

func (m *mockRemoteWS) broadcast(msg node.ServerMsg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mc := range m.conns {
		mc.writeJSON(msg)
	}
}

// ─── wsRelay tests ───────────────────────────────────────────────────────────

func TestWSRelay_ConnectAndSubscribe(t *testing.T) {
	mock := newMockRemoteWS("secret")
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nc := node.NewHTTPClient("remote", ts.URL, "secret", "Remote")
	defer nc.Close()

	client := newTestWSClient()
	nc.Subscribe(client, "test:d:u:general", 0)

	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "subscribed" {
		t.Errorf("type = %q, want subscribed", msg.Type)
	}
	if msg.Key != "test:d:u:general" {
		t.Errorf("key = %q", msg.Key)
	}
	if msg.Node != "remote" {
		t.Errorf("node = %q, want remote", msg.Node)
	}
}

func TestWSRelay_EventForwarding(t *testing.T) {
	mock := newMockRemoteWS("")
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nc := node.NewHTTPClient("remote", ts.URL, "", "Remote")
	defer nc.Close()

	client := newTestWSClient()
	nc.Subscribe(client, "test:d:u:general", 0)
	_ = readClientMsg(t, client, 2*time.Second) // subscribed

	// Wait for readLoop to start
	time.Sleep(100 * time.Millisecond)

	// Send event from remote
	mock.broadcast(node.ServerMsg{
		Type:  "event",
		Key:   "test:d:u:general",
		Event: &cli.EventEntry{Time: 1000, Type: "text", Summary: "hello"},
	})

	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "event" {
		t.Errorf("type = %q, want event", msg.Type)
	}
	if msg.Node != "remote" {
		t.Errorf("node = %q, want remote", msg.Node)
	}
	if msg.Event == nil || msg.Event.Summary != "hello" {
		t.Error("event data mismatch")
	}
}

func TestWSRelay_MultipleClients(t *testing.T) {
	mock := newMockRemoteWS("")
	mock.apiEvents = []cli.EventEntry{{Time: 500, Type: "init", Summary: "init"}}
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nc := node.NewHTTPClient("remote", ts.URL, "", "Remote")
	defer nc.Close()

	client1 := newTestWSClient()
	nc.Subscribe(client1, "test:d:u:general", 0)
	_ = readClientMsg(t, client1, 2*time.Second) // subscribed

	// Second client subscribes to same key (uses HTTP history path)
	client2 := newTestWSClient()
	nc.Subscribe(client2, "test:d:u:general", 0)
	msg2 := readClientMsg(t, client2, 2*time.Second)
	if msg2.Type != "subscribed" {
		t.Errorf("client2: type = %q, want subscribed", msg2.Type)
	}

	// Client2 should also get history via HTTP
	hist := readClientMsg(t, client2, 2*time.Second)
	if hist.Type != "history" {
		t.Errorf("client2 history: type = %q, want history", hist.Type)
	}

	// Wait for readLoop
	time.Sleep(100 * time.Millisecond)

	// Send event from remote, both should receive
	mock.broadcast(node.ServerMsg{
		Type:  "event",
		Key:   "test:d:u:general",
		Event: &cli.EventEntry{Time: 2000, Type: "text", Summary: "shared"},
	})

	e1 := readClientMsg(t, client1, 2*time.Second)
	e2 := readClientMsg(t, client2, 2*time.Second)
	if e1.Type != "event" {
		t.Errorf("client1: type = %q, want event", e1.Type)
	}
	if e2.Type != "event" {
		t.Errorf("client2: type = %q, want event", e2.Type)
	}
}

func TestWSRelay_Unsubscribe(t *testing.T) {
	mock := newMockRemoteWS("")
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nc := node.NewHTTPClient("remote", ts.URL, "", "Remote")
	defer nc.Close()

	client := newTestWSClient()
	nc.Subscribe(client, "test:d:u:general", 0)
	_ = readClientMsg(t, client, 2*time.Second) // subscribed

	nc.Unsubscribe(client, "test:d:u:general")
	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "unsubscribed" {
		t.Errorf("type = %q, want unsubscribed", msg.Type)
	}

	// Verify remote got unsubscribe
	time.Sleep(200 * time.Millisecond)
	mock.mu.Lock()
	_, subbed := mock.subKeys["test:d:u:general"]
	mock.mu.Unlock()
	if subbed {
		t.Error("remote should have been unsubscribed")
	}
}

func TestWSRelay_Close(t *testing.T) {
	mock := newMockRemoteWS("")
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nc := node.NewHTTPClient("remote", ts.URL, "", "Remote")

	client := newTestWSClient()
	nc.Subscribe(client, "test:d:u:general", 0)
	_ = readClientMsg(t, client, 2*time.Second)

	nc.Close()

	// After Close, subscribing should still not panic (idempotent close).
	nc.Close()
}

func TestWSRelay_Reconnect(t *testing.T) {
	mock := newMockRemoteWS("")
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nc := node.NewHTTPClient("remote", ts.URL, "", "Remote")
	defer nc.Close()

	client := newTestWSClient()
	nc.Subscribe(client, "test:d:u:general", 0)
	_ = readClientMsg(t, client, 2*time.Second) // subscribed

	// Close the remote connection to trigger reconnect
	mock.mu.Lock()
	for _, mc := range mock.conns {
		mc.conn.Close()
	}
	mock.conns = nil
	mock.mu.Unlock()

	// Wait for reconnect (1s initial backoff)
	time.Sleep(3 * time.Second)

	// Verify reconnect by checking the mock received new connections
	mock.mu.Lock()
	reconnected := len(mock.conns) > 0
	mock.mu.Unlock()
	if !reconnected {
		t.Error("relay should have reconnected")
	}
}

func TestWSRelay_AuthFailed(t *testing.T) {
	mock := newMockRemoteWS("correct-token")
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nc := node.NewHTTPClient("remote", ts.URL, "wrong-token", "Remote")
	defer nc.Close()

	client := newTestWSClient()
	nc.Subscribe(client, "test:d:u:general", 0)

	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "error" {
		t.Errorf("type = %q, want error", msg.Type)
	}
}

func TestWSRelay_RemoveClient(t *testing.T) {
	mock := newMockRemoteWS("")
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nc := node.NewHTTPClient("remote", ts.URL, "", "Remote")
	defer nc.Close()

	client := newTestWSClient()
	nc.Subscribe(client, "test:d:u:general", 0)
	_ = readClientMsg(t, client, 2*time.Second)

	nc.RemoveClient(client)

	// Allow time for the unsubscribe to propagate to the remote mock
	time.Sleep(200 * time.Millisecond)
}

// ─── Hub remote subscribe integration ────────────────────────────────────────

func TestHub_RemoteSubscribe(t *testing.T) {
	mock := newMockRemoteWS("")
	ts := httptest.NewServer(mock.handler)
	defer ts.Close()

	nodes := map[string]node.Conn{
		"remote": node.NewHTTPClient("remote", ts.URL, "", "Remote"),
	}
	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{Router: router, Guard: guard, Nodes: nodes, NodesMu: &nodesMu})
	defer hub.Shutdown()

	client := newTestWSClient()
	hub.mu.Lock()
	hub.clients[client] = struct{}{}
	hub.mu.Unlock()

	hub.handleSubscribe(client, node.ClientMsg{
		Type: "subscribe",
		Key:  "test:d:u:general",
		Node: "remote",
	})

	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "subscribed" {
		t.Errorf("type = %q, want subscribed", msg.Type)
	}
	if msg.Node != "remote" {
		t.Errorf("node = %q, want remote", msg.Node)
	}
}

func TestHub_RemoteSubscribe_UnknownNode(t *testing.T) {
	hub, _ := newTestHub("")
	client := newTestWSClient()

	hub.handleSubscribe(client, node.ClientMsg{
		Type: "subscribe",
		Key:  "test:d:u:general",
		Node: "nonexistent",
	})

	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "error" {
		t.Errorf("type = %q, want error", msg.Type)
	}
}

func TestHub_RemoteUnsubscribe_NoRelay(t *testing.T) {
	hub, _ := newTestHub("")
	client := newTestWSClient()

	hub.handleUnsubscribe(client, node.ClientMsg{
		Type: "unsubscribe",
		Key:  "test:d:u:general",
		Node: "remote",
	})

	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "unsubscribed" {
		t.Errorf("type = %q, want unsubscribed", msg.Type)
	}
}
