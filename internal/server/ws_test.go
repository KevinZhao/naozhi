package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// testCookieSecret is a fixed HMAC key for deterministic test cookie values.
var testCookieSecret = []byte("test-cookie-secret-key-for-hmac!")

func testCookieMAC(token string) string {
	if token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, testCookieSecret)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

func newTestHub(token string) (*Hub, *session.Router) {
	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{Router: router, DashToken: token, CookieMAC: testCookieMAC(token), Guard: guard, NodesMu: &nodesMu})
	return hub, router
}

func newTestHubWithAgents(token string, agents map[string]session.AgentOpts) (*Hub, *session.Router) {
	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{Router: router, Agents: agents, DashToken: token, CookieMAC: testCookieMAC(token), Guard: guard, NodesMu: &nodesMu})
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

func wsWrite(t *testing.T, conn *websocket.Conn, msg node.ClientMsg) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func wsRead(t *testing.T, conn *websocket.Conn) node.ServerMsg {
	t.Helper()
	var resp node.ServerMsg
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

	wsWrite(t, conn, node.ClientMsg{Type: "auth", Token: "secret"})
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

	wsWrite(t, conn, node.ClientMsg{Type: "auth", Token: "wrong"})
	resp := wsRead(t, conn)

	if resp.Type != "auth_fail" {
		t.Errorf("type = %q, want auth_fail", resp.Type)
	}
	if resp.Error == "" {
		t.Error("expected non-empty error message on auth_fail")
	}
}

func TestWS_AuthCookiePreAuth(t *testing.T) {
	hub, _ := newTestHub("secret")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	// Dial with valid HMAC cookie — simulates iOS where localStorage is empty but cookie persists
	header := http.Header{}
	header.Set("Cookie", authCookieName+"="+testCookieMAC("secret"))
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Send auth with empty token — should still succeed via cookie pre-auth
	wsWrite(t, conn, node.ClientMsg{Type: "auth", Token: ""})
	resp := wsRead(t, conn)

	if resp.Type != "auth_ok" {
		t.Errorf("type = %q, want auth_ok (cookie pre-auth should accept)", resp.Type)
	}
}

func TestWS_AuthNotRequired(t *testing.T) {
	hub, _ := newTestHub("") // no token required
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// Should be able to use commands without auth
	wsWrite(t, conn, node.ClientMsg{Type: "ping"})
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
	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
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

	wsWrite(t, conn, node.ClientMsg{Type: "ping"})
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

	// Server returns "session not found" immediately for non-existent sessions.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "nonexistent:d:u:general"})
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

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe"})
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

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})

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

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general", After: 1500})

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

// TestWS_SubscribeWithLimit verifies the dashboard pagination fast path:
// a fresh subscribe with limit=N only receives the newest N events instead
// of the full event log.
func TestWS_SubscribeWithLimit(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	for i := 1; i <= 5; i++ {
		proc.EventLog.Append(cli.EventEntry{Time: int64(i * 1000), Type: "text", Summary: "msg"})
	}
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general", Limit: 2})

	resp := wsRead(t, conn)
	if resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}

	resp = wsRead(t, conn)
	if resp.Type != "history" {
		t.Fatalf("type = %q, want history", resp.Type)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2 (limit should keep only newest)", len(resp.Events))
	}
	// Chronological order: newest two are time=4000 and time=5000.
	if resp.Events[0].Time != 4000 || resp.Events[1].Time != 5000 {
		t.Errorf("events times = [%d, %d], want [4000, 5000]",
			resp.Events[0].Time, resp.Events[1].Time)
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

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})

	// Read subscribed (no history since log is empty)
	resp := wsRead(t, conn)
	if resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}

	// Now append an event
	proc.EventLog.Append(cli.EventEntry{Time: time.Now().UnixMilli(), Type: "thinking", Summary: "reasoning"})

	// Should receive the push (batched as history)
	resp = wsRead(t, conn)
	if resp.Type != "history" {
		t.Fatalf("type = %q, want history", resp.Type)
	}
	if resp.Key != "test:d:u:general" {
		t.Errorf("key = %q, want test:d:u:general", resp.Key)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	if resp.Events[0].Type != "thinking" {
		t.Errorf("event.Type = %q, want thinking", resp.Events[0].Type)
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

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn) // subscribed

	// Append multiple events
	now := time.Now().UnixMilli()
	proc.EventLog.Append(cli.EventEntry{Time: now, Type: "thinking", Summary: "step1"})
	proc.EventLog.Append(cli.EventEntry{Time: now + 1, Type: "tool_use", Summary: "Read", Tool: "Read"})

	// Should receive both events (possibly in one or two history batches)
	var received []cli.EventEntry
	for len(received) < 2 {
		resp := wsRead(t, conn)
		if resp.Type == "history" {
			received = append(received, resp.Events...)
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

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn) // subscribed

	wsWrite(t, conn, node.ClientMsg{Type: "unsubscribe", Key: "test:d:u:general"})
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

	wsWrite(t, conn, node.ClientMsg{Type: "send", Key: "test:d:u:general", Text: "hello", ID: "req-1"})
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

	// Pre-acquire the guard — new message will interrupt and wait
	hub.guard.TryAcquire(key)
	defer hub.guard.Release(key)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "send", Key: key, Text: "hello", ID: "req-2"})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" {
		t.Fatalf("type = %q, want send_ack", resp.Type)
	}
	// With interrupt-on-busy, the immediate ack is "accepted";
	// the goroutine will eventually timeout waiting for the guard.
	if resp.Status != "accepted" {
		t.Errorf("status = %q, want accepted", resp.Status)
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

	wsWrite(t, conn, node.ClientMsg{Type: "send", Text: "hello"})
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

	wsWrite(t, conn, node.ClientMsg{Type: "send", Key: "test:d:u:general"})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" {
		t.Fatalf("type = %q, want send_ack", resp.Type)
	}
	if resp.Status != "error" {
		t.Errorf("status = %q, want error", resp.Status)
	}
}

// Remote-node send must enforce the same per-field text cap as the local
// send path. Without this guard an authenticated dashboard user who
// targets a remote node can bypass the local cap and push up to
// wsMaxMessageSize bytes into nc.Send, amplifying input into the remote
// shim's 12 MB stdin line ceiling via coalesce at the remote. R62-SEC-1.
func TestWS_RemoteSendTextTooLong(t *testing.T) {
	hub, _ := newTestHub("")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	big := strings.Repeat("x", maxWSSendTextBytes+1)
	// Node name is syntactically valid (isValidNodeID) but not registered
	// on the hub. The cap check must fire before the unknown-node lookup
	// so oversized text is rejected even when the target node is missing,
	// matching the defence-in-depth shape of handleSend.
	wsWrite(t, conn, node.ClientMsg{Type: "send", Node: "macbook", Key: "test:d:u:general", Text: big})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" || resp.Status != "error" {
		t.Fatalf("resp = %+v, want send_ack/error", resp)
	}
	if !strings.Contains(resp.Error, "too long") {
		t.Errorf("error = %q, want 'too long'", resp.Error)
	}
}

// Text oversize is rejected with a clear error instead of silently getting
// truncated or accepted then coalesced into a multi-MB CLI stdin write. The
// dispatch queue's coalescing depth is bounded but the per-message cap is
// what prevents a single send from dominating memory. R59-SEC-H1.
func TestWS_SendTextTooLong(t *testing.T) {
	hub, _ := newTestHub("")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// One byte past the cap — enough to prove the boundary without bumping
	// the ws read limit.
	big := strings.Repeat("x", maxWSSendTextBytes+1)
	wsWrite(t, conn, node.ClientMsg{Type: "send", Key: "test:d:u:general", Text: big})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" || resp.Status != "error" {
		t.Fatalf("resp = %+v, want send_ack/error", resp)
	}
	if !strings.Contains(resp.Error, "too long") {
		t.Errorf("error = %q, want 'too long'", resp.Error)
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

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
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
	wsWrite(t, conn1, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn1) // subscribed

	wsWrite(t, conn2, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn2) // subscribed

	// Append event
	proc.EventLog.Append(cli.EventEntry{Time: time.Now().UnixMilli(), Type: "text", Summary: "shared event"})

	// Both should receive it
	var wg sync.WaitGroup
	wg.Add(2)

	check := func(conn *websocket.Conn, label string) {
		defer wg.Done()
		var resp node.ServerMsg
		if err := conn.ReadJSON(&resp); err != nil {
			t.Errorf("%s: read error: %v", label, err)
			return
		}
		if resp.Type != "history" {
			t.Errorf("%s: type = %q, want history", label, resp.Type)
		}
		if len(resp.Events) == 0 || resp.Events[0].Summary != "shared event" {
			t.Errorf("%s: unexpected events: %+v", label, resp.Events)
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

	wsWrite(t, conn, node.ClientMsg{Type: "ping"})
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
	wsWrite(t, conn, node.ClientMsg{Type: "auth", Token: "tok"})
	resp := wsRead(t, conn)
	if resp.Type != "auth_ok" {
		t.Fatalf("auth: type = %q, want auth_ok", resp.Type)
	}

	// 2. Subscribe
	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
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

	// 3. Event push (batched as history)
	proc.EventLog.Append(cli.EventEntry{Time: time.Now().UnixMilli(), Type: "thinking", Summary: "reasoning..."})
	resp = wsRead(t, conn)
	if resp.Type != "history" {
		t.Fatalf("push: type = %q, want history", resp.Type)
	}
	if len(resp.Events) != 1 || resp.Events[0].Type != "thinking" {
		t.Errorf("push events: %+v", resp.Events)
	}

	// 4. Unsubscribe
	wsWrite(t, conn, node.ClientMsg{Type: "unsubscribe", Key: "test:d:u:general"})
	resp = wsRead(t, conn)
	if resp.Type != "unsubscribed" {
		t.Errorf("unsub: type = %q, want unsubscribed", resp.Type)
	}
}

// ─── node.ServerMsg JSON roundtrip ──────────────────────────────────────────────

func TestWsServerMsg_JSONRoundtrip(t *testing.T) {
	msg := node.ServerMsg{
		Type:  "event",
		Key:   "test:d:u:general",
		Event: &cli.EventEntry{Time: 1000, Type: "text", Summary: "hello", Detail: "hello world", Tool: ""},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed node.ServerMsg
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

// TestHandleAuth_WSToken_SetsUploadOwner locks down R67-SEC-1: successful
// WS token-auth must derive uploadOwner from the provided token so the
// per-owner upload quota is enforced. Before the fix, c.uploadOwner stayed
// "" and any WS-token-authed client could bypass maxUploadPerOwner.
func TestHandleAuth_WSToken_SetsUploadOwner(t *testing.T) {
	hub, _ := newTestHub("secret")
	defer hub.Shutdown()

	c := &wsClient{
		send:          make(chan []byte, 4),
		done:          make(chan struct{}),
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	hub.handleAuth(c, node.ClientMsg{Type: "auth", Token: "secret"})

	if !c.authenticated.Load() {
		t.Fatal("expected authenticated=true after valid token")
	}
	if c.uploadOwner == "" {
		t.Fatal("uploadOwner is empty — per-owner upload quota cannot be enforced (R67-SEC-1)")
	}
	// Exactly hex(sha256("secret")[:8]) — 16 hex chars = 8 bytes.
	if len(c.uploadOwner) != 16 {
		t.Errorf("uploadOwner length = %d, want 16 (hex of 8-byte prefix)", len(c.uploadOwner))
	}
}

// TestHandleAuth_WSToken_OwnerStableAcrossCalls verifies the derivation is
// deterministic so that files uploaded under the same token from HTTP and
// WS can cross-claim in the upload store.
func TestHandleAuth_WSToken_OwnerStableAcrossCalls(t *testing.T) {
	hub, _ := newTestHub("secret")
	defer hub.Shutdown()

	derive := func() string {
		c := &wsClient{
			send:          make(chan []byte, 4),
			done:          make(chan struct{}),
			subscriptions: make(map[string]func()),
			subGen:        make(map[string]uint64),
		}
		hub.handleAuth(c, node.ClientMsg{Type: "auth", Token: "secret"})
		return c.uploadOwner
	}
	if a, b := derive(), derive(); a != b {
		t.Errorf("uploadOwner not stable: %q vs %q", a, b)
	}
}
