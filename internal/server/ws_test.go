package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func newTestHub(token string) (*Hub, *session.Router) {
	router := session.NewRouter(session.RouterConfig{})
	guard := newSessionGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(router, nil, nil, token, guard, nil, &nodesMu, nil, "")
	return hub, router
}

func newTestHubWithAgents(token string, agents map[string]session.AgentOpts) (*Hub, *session.Router) {
	router := session.NewRouter(session.RouterConfig{})
	guard := newSessionGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(router, agents, nil, token, guard, nil, &nodesMu, nil, "")
	return hub, router
}

func startWSServer(t *testing.T, hub *Hub) (string, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", hub.HandleUpgrade)
	ts := httptest.NewServer(mux)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	return wsURL, func() {
		hub.Shutdown()
		ts.Close()
	}
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return conn
}

func wsWrite(t *testing.T, conn *websocket.Conn, msg wsClientMsg) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func wsRead(t *testing.T, conn *websocket.Conn) wsServerMsg {
	t.Helper()
	var resp wsServerMsg
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	return resp
}

// ─── Auth tests ──────────────────────────────────────────────────────────────

func TestWS_AuthOK(t *testing.T) {
	hub, _ := newTestHub("secret")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "auth", Token: "secret"})
	resp := wsRead(t, conn)

	if resp.Type != "auth_ok" {
		t.Errorf("type = %q, want auth_ok", resp.Type)
	}
}

func TestWS_AuthFail(t *testing.T) {
	hub, _ := newTestHub("secret")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "auth", Token: "wrong"})
	resp := wsRead(t, conn)

	if resp.Type != "auth_fail" {
		t.Errorf("type = %q, want auth_fail", resp.Type)
	}
	if resp.Error == "" {
		t.Error("expected non-empty error message on auth_fail")
	}
}

func TestWS_AuthNotRequired(t *testing.T) {
	hub, _ := newTestHub("") // no token required
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// Should be able to use commands without auth
	wsWrite(t, conn, wsClientMsg{Type: "ping"})
	resp := wsRead(t, conn)

	if resp.Type != "pong" {
		t.Errorf("type = %q, want pong", resp.Type)
	}
}

func TestWS_UnauthenticatedCommandRejected(t *testing.T) {
	hub, _ := newTestHub("secret")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// Try subscribe without auth
	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	resp := wsRead(t, conn)

	if resp.Type != "error" {
		t.Errorf("type = %q, want error", resp.Type)
	}
	if !strings.Contains(resp.Error, "not authenticated") {
		t.Errorf("error = %q, want 'not authenticated'", resp.Error)
	}
}

// ─── Ping/Pong test ─────────────────────────────────────────────────────────

func TestWS_Ping(t *testing.T) {
	hub, _ := newTestHub("")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "ping"})
	resp := wsRead(t, conn)

	if resp.Type != "pong" {
		t.Errorf("type = %q, want pong", resp.Type)
	}
}

// ─── Subscribe tests ─────────────────────────────────────────────────────────

func TestWS_SubscribeSessionNotFound(t *testing.T) {
	hub, _ := newTestHub("")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "nonexistent:d:u:general"})
	resp := wsRead(t, conn)

	if resp.Type != "error" {
		t.Errorf("type = %q, want error", resp.Type)
	}
	if !strings.Contains(resp.Error, "session not found") {
		t.Errorf("error = %q, want 'session not found'", resp.Error)
	}
}

func TestWS_SubscribeMissingKey(t *testing.T) {
	hub, _ := newTestHub("")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "subscribe"})
	resp := wsRead(t, conn)

	if resp.Type != "error" {
		t.Errorf("type = %q, want error", resp.Type)
	}
}

func TestWS_SubscribeAndHistory(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	proc.EventLog.Append(cli.EventEntry{Time: 1000, Type: "system", Summary: "init"})
	proc.EventLog.Append(cli.EventEntry{Time: 2000, Type: "text", Summary: "hello"})
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})

	// First message: subscribed
	resp := wsRead(t, conn)
	if resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}
	if resp.Key != "test:d:u:general" {
		t.Errorf("key = %q, want test:d:u:general", resp.Key)
	}
	if resp.State == "" {
		t.Error("expected non-empty state")
	}

	// Second message: history
	resp = wsRead(t, conn)
	if resp.Type != "history" {
		t.Fatalf("type = %q, want history", resp.Type)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(resp.Events))
	}
	if resp.Events[0].Type != "system" || resp.Events[1].Type != "text" {
		t.Errorf("events = %+v", resp.Events)
	}
}

func TestWS_SubscribeWithAfter(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	proc.EventLog.Append(cli.EventEntry{Time: 1000, Type: "system", Summary: "init"})
	proc.EventLog.Append(cli.EventEntry{Time: 2000, Type: "text", Summary: "hello"})
	proc.EventLog.Append(cli.EventEntry{Time: 3000, Type: "result", Summary: "done"})
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "test:d:u:general", After: 1500})

	// subscribed
	resp := wsRead(t, conn)
	if resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}

	// history with only events after 1500
	resp = wsRead(t, conn)
	if resp.Type != "history" {
		t.Fatalf("type = %q, want history", resp.Type)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2 (after=1500 should exclude time=1000)", len(resp.Events))
	}
}

// ─── Event push tests ────────────────────────────────────────────────────────

func TestWS_EventPush(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})

	// Read subscribed (no history since log is empty)
	resp := wsRead(t, conn)
	if resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}

	// Now append an event
	proc.EventLog.Append(cli.EventEntry{Time: time.Now().UnixMilli(), Type: "thinking", Summary: "reasoning"})

	// Should receive the push
	resp = wsRead(t, conn)
	if resp.Type != "event" {
		t.Fatalf("type = %q, want event", resp.Type)
	}
	if resp.Key != "test:d:u:general" {
		t.Errorf("key = %q, want test:d:u:general", resp.Key)
	}
	if resp.Event == nil {
		t.Fatal("event should not be nil")
	}
	if resp.Event.Type != "thinking" {
		t.Errorf("event.Type = %q, want thinking", resp.Event.Type)
	}
}

func TestWS_EventPushMultiple(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn) // subscribed

	// Append multiple events
	now := time.Now().UnixMilli()
	proc.EventLog.Append(cli.EventEntry{Time: now, Type: "thinking", Summary: "step1"})
	proc.EventLog.Append(cli.EventEntry{Time: now + 1, Type: "tool_use", Summary: "Read", Tool: "Read"})

	// Should receive both events
	var received []wsServerMsg
	for i := 0; i < 2; i++ {
		resp := wsRead(t, conn)
		if resp.Type == "event" {
			received = append(received, resp)
		}
	}

	if len(received) < 2 {
		t.Fatalf("received %d events, want 2", len(received))
	}
}

// ─── Unsubscribe test ────────────────────────────────────────────────────────

func TestWS_Unsubscribe(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn) // subscribed

	wsWrite(t, conn, wsClientMsg{Type: "unsubscribe", Key: "test:d:u:general"})
	resp := wsRead(t, conn)

	if resp.Type != "unsubscribed" {
		t.Errorf("type = %q, want unsubscribed", resp.Type)
	}
	if resp.Key != "test:d:u:general" {
		t.Errorf("key = %q, want test:d:u:general", resp.Key)
	}
}

// ─── Send tests ──────────────────────────────────────────────────────────────

func TestWS_SendAccepted(t *testing.T) {
	hub, router := newTestHubWithAgents("", nil)
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "send", Key: "test:d:u:general", Text: "hello", ID: "req-1"})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" {
		t.Fatalf("type = %q, want send_ack", resp.Type)
	}
	if resp.Status != "accepted" {
		t.Errorf("status = %q, want accepted", resp.Status)
	}
	if resp.ID != "req-1" {
		t.Errorf("id = %q, want req-1", resp.ID)
	}
	if resp.Key != "test:d:u:general" {
		t.Errorf("key = %q, want test:d:u:general", resp.Key)
	}
}

func TestWS_SendBusy(t *testing.T) {
	hub, _ := newTestHub("")
	key := "test:d:u:general"

	// Pre-acquire the guard
	hub.guard.TryAcquire(key)
	defer hub.guard.Release(key)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "send", Key: key, Text: "hello", ID: "req-2"})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" {
		t.Fatalf("type = %q, want send_ack", resp.Type)
	}
	if resp.Status != "busy" {
		t.Errorf("status = %q, want busy", resp.Status)
	}
	if resp.ID != "req-2" {
		t.Errorf("id = %q, want req-2", resp.ID)
	}
}

func TestWS_SendMissingKey(t *testing.T) {
	hub, _ := newTestHub("")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "send", Text: "hello"})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" {
		t.Fatalf("type = %q, want send_ack", resp.Type)
	}
	if resp.Status != "error" {
		t.Errorf("status = %q, want error", resp.Status)
	}
}

func TestWS_SendMissingText(t *testing.T) {
	hub, _ := newTestHub("")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "send", Key: "test:d:u:general"})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" {
		t.Fatalf("type = %q, want send_ack", resp.Type)
	}
	if resp.Status != "error" {
		t.Errorf("status = %q, want error", resp.Status)
	}
}

// ─── Client disconnect cleanup ──────────────────────────────────────────────

func TestWS_ClientDisconnectCleanup(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)

	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn) // subscribed

	// Close connection
	conn.Close()

	// Give time for cleanup
	time.Sleep(100 * time.Millisecond)

	hub.mu.Lock()
	clientCount := len(hub.clients)
	hub.mu.Unlock()

	if clientCount != 0 {
		t.Errorf("client count = %d after disconnect, want 0", clientCount)
	}
}

// ─── Multiple clients ────────────────────────────────────────────────────────

func TestWS_MultipleClientsReceiveEvents(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	// Connect two clients
	conn1 := dialWS(t, url)
	defer conn1.Close()
	conn2 := dialWS(t, url)
	defer conn2.Close()

	// Both subscribe
	wsWrite(t, conn1, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn1) // subscribed

	wsWrite(t, conn2, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn2) // subscribed

	// Append event
	proc.EventLog.Append(cli.EventEntry{Time: time.Now().UnixMilli(), Type: "text", Summary: "shared event"})

	// Both should receive it
	var wg sync.WaitGroup
	wg.Add(2)

	check := func(conn *websocket.Conn, label string) {
		defer wg.Done()
		var resp wsServerMsg
		if err := conn.ReadJSON(&resp); err != nil {
			t.Errorf("%s: read error: %v", label, err)
			return
		}
		if resp.Type != "event" {
			t.Errorf("%s: type = %q, want event", label, resp.Type)
		}
		if resp.Event == nil || resp.Event.Summary != "shared event" {
			t.Errorf("%s: unexpected event: %+v", label, resp.Event)
		}
	}

	go check(conn1, "client1")
	go check(conn2, "client2")
	wg.Wait()
}

// ─── Hub shutdown ────────────────────────────────────────────────────────────

func TestWS_HubShutdown(t *testing.T) {
	hub, _ := newTestHub("")
	url, _ := startWSServer(t, hub)

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, wsClientMsg{Type: "ping"})
	_ = wsRead(t, conn) // pong

	hub.Shutdown()

	hub.mu.Lock()
	clientCount := len(hub.clients)
	hub.mu.Unlock()

	if clientCount != 0 {
		t.Errorf("client count = %d after shutdown, want 0", clientCount)
	}
}

// ─── Integration: auth + subscribe + event push ──────────────────────────────

func TestWS_FullFlow(t *testing.T) {
	hub, router := newTestHub("tok")
	proc := session.NewTestProcess()
	proc.EventLog.Append(cli.EventEntry{Time: 1000, Type: "system", Summary: "init"})
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// 1. Auth
	wsWrite(t, conn, wsClientMsg{Type: "auth", Token: "tok"})
	resp := wsRead(t, conn)
	if resp.Type != "auth_ok" {
		t.Fatalf("auth: type = %q, want auth_ok", resp.Type)
	}

	// 2. Subscribe
	wsWrite(t, conn, wsClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	resp = wsRead(t, conn) // subscribed
	if resp.Type != "subscribed" {
		t.Fatalf("subscribe: type = %q, want subscribed", resp.Type)
	}
	resp = wsRead(t, conn) // history
	if resp.Type != "history" {
		t.Fatalf("history: type = %q, want history", resp.Type)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("history events = %d, want 1", len(resp.Events))
	}

	// 3. Event push
	proc.EventLog.Append(cli.EventEntry{Time: time.Now().UnixMilli(), Type: "thinking", Summary: "reasoning..."})
	resp = wsRead(t, conn)
	if resp.Type != "event" {
		t.Fatalf("push: type = %q, want event", resp.Type)
	}
	if resp.Event.Type != "thinking" {
		t.Errorf("push event.Type = %q, want thinking", resp.Event.Type)
	}

	// 4. Unsubscribe
	wsWrite(t, conn, wsClientMsg{Type: "unsubscribe", Key: "test:d:u:general"})
	resp = wsRead(t, conn)
	if resp.Type != "unsubscribed" {
		t.Errorf("unsub: type = %q, want unsubscribed", resp.Type)
	}
}

// ─── wsServerMsg JSON roundtrip ──────────────────────────────────────────────

func TestWsServerMsg_JSONRoundtrip(t *testing.T) {
	msg := wsServerMsg{
		Type:  "event",
		Key:   "test:d:u:general",
		Event: &cli.EventEntry{Time: 1000, Type: "text", Summary: "hello", Detail: "hello world", Tool: ""},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed wsServerMsg
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Type != "event" || parsed.Key != "test:d:u:general" {
		t.Errorf("roundtrip failed: %+v", parsed)
	}
	if parsed.Event == nil || parsed.Event.Type != "text" {
		t.Errorf("event roundtrip failed: %+v", parsed.Event)
	}
}
