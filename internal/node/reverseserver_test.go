package node

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
)

// dialReverseNode dials the /ws-node endpoint and returns the connection.
func dialReverseNode(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws-node"
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial ws-node: %v", err)
	}
	return conn
}

// reverseAuth performs the register handshake and returns the server's response.
func reverseAuth(t *testing.T, conn *websocket.Conn, nodeID, token, hostname string) ReverseMsg {
	t.Helper()
	err := conn.WriteJSON(ReverseMsg{
		Type:     "register",
		NodeID:   nodeID,
		Token:    token,
		Hostname: hostname,
	})
	if err != nil {
		t.Fatalf("write register: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp ReverseMsg
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("read register response: %v", err)
	}
	return resp
}

// newTestReverseServer creates a ReverseServer with a single authorized node.
func newTestReverseServer(nodeID, token string, trustedProxy bool) *ReverseServer {
	auth := map[string]config.ReverseNodeEntry{
		nodeID: {Token: token, DisplayName: "Test Node"},
	}
	return NewReverseServer(auth, trustedProxy)
}

// ---- Happy-path registration ----

func TestReverseServer_Register_ok(t *testing.T) {
	rs := newTestReverseServer("node-1", "secret", false)

	var registered atomic.Bool
	rs.OnRegister = func(id string, conn *ReverseConn) {
		if id == "node-1" {
			registered.Store(true)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	defer conn.Close()

	resp := reverseAuth(t, conn, "node-1", "secret", "worker.internal")
	if resp.Type != "registered" {
		t.Fatalf("expected 'registered', got %q (err: %q)", resp.Type, resp.Error)
	}

	// Give OnRegister a moment to be called.
	time.Sleep(30 * time.Millisecond)
	if !registered.Load() {
		t.Error("OnRegister was not called")
	}
}

// ---- Wrong token is rejected ----

func TestReverseServer_Register_wrongToken(t *testing.T) {
	rs := newTestReverseServer("node-1", "correct", false)

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	defer conn.Close()

	resp := reverseAuth(t, conn, "node-1", "wrong", "host")
	if resp.Type != "register_fail" {
		t.Fatalf("expected 'register_fail', got %q", resp.Type)
	}
}

// ---- Unknown node_id is rejected ----

func TestReverseServer_Register_unknownNodeID(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	defer conn.Close()

	resp := reverseAuth(t, conn, "unknown-node", "tok", "host")
	if resp.Type != "register_fail" {
		t.Fatalf("expected 'register_fail', got %q", resp.Type)
	}
}

// ---- Wrong first message type causes close ----

func TestReverseServer_Register_wrongMsgType(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	defer conn.Close()

	// Send wrong message type.
	conn.WriteJSON(ReverseMsg{Type: "hello", NodeID: "node-1", Token: "tok"})
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("expected connection to be closed after wrong message type")
	}
}

// ---- Browser Origin header causes rejection ----

func TestReverseServer_Register_originRejected(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws-node"
	hdr := http.Header{"Origin": []string{"http://evil.example.com"}}
	_, resp, err := websocket.DefaultDialer.Dial(u, hdr)
	if err == nil {
		t.Fatal("expected dial to fail when Origin header is present")
	}
	if resp != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("expected non-101 response")
	}
}

// ---- AllNodes returns configured nodes ----

func TestReverseServer_AllNodes(t *testing.T) {
	auth := map[string]config.ReverseNodeEntry{
		"node-a": {Token: "t1", DisplayName: "Node A"},
		"node-b": {Token: "t2", DisplayName: "Node B"},
	}
	rs := NewReverseServer(auth, false)
	nodes := rs.AllNodes()
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes["node-a"] != "Node A" {
		t.Errorf("node-a display name: want 'Node A', got %q", nodes["node-a"])
	}
	if nodes["node-b"] != "Node B" {
		t.Errorf("node-b display name: want 'Node B', got %q", nodes["node-b"])
	}
}

// ---- AllNodes includes disconnected nodes ----

func TestReverseServer_AllNodes_includesDisconnected(t *testing.T) {
	auth := map[string]config.ReverseNodeEntry{
		"node-1": {Token: "tok"},
	}
	rs := NewReverseServer(auth, false)
	nodes := rs.AllNodes()
	if _, ok := nodes["node-1"]; !ok {
		t.Error("expected node-1 in AllNodes even without active connection")
	}
}

// ---- Reconnect with same node_id closes old connection ----

func TestReverseServer_Reconnect_closesOldConn(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	var registerCount atomic.Int32
	rs.OnRegister = func(id string, conn *ReverseConn) { registerCount.Add(1) }

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn1 := dialReverseNode(t, srv)
	resp1 := reverseAuth(t, conn1, "node-1", "tok", "host")
	if resp1.Type != "registered" {
		t.Fatalf("conn1: expected registered, got %q", resp1.Type)
	}

	conn2 := dialReverseNode(t, srv)
	resp2 := reverseAuth(t, conn2, "node-1", "tok", "host")
	if resp2.Type != "registered" {
		t.Fatalf("conn2: expected registered, got %q", resp2.Type)
	}
	defer conn2.Close()

	// conn1 should be closed by the server.
	conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn1.ReadMessage()
	if err == nil {
		t.Error("expected conn1 to be closed after reconnect with same node_id")
	}

	time.Sleep(30 * time.Millisecond)
	if registerCount.Load() != 2 {
		t.Errorf("expected OnRegister called twice, got %d", registerCount.Load())
	}
}

// ---- OnDeregister is called when connection drops ----

func TestReverseServer_OnDeregister_calledOnDisconnect(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	deregistered := make(chan string, 1)
	rs.OnDeregister = func(id string) { deregistered <- id }

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	resp := reverseAuth(t, conn, "node-1", "tok", "host")
	if resp.Type != "registered" {
		conn.Close()
		t.Fatalf("expected registered, got %q", resp.Type)
	}

	// Close the client side to trigger deregister.
	conn.Close()

	select {
	case id := <-deregistered:
		if id != "node-1" {
			t.Errorf("expected deregister for 'node-1', got %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for OnDeregister")
	}
}

// ---- Rate limiter rejects rapid reconnects from same IP ----

func TestReverseServer_RateLimiter_rejectsRapidConnects(t *testing.T) {
	// The wsLimiter has burst=10, rate=1/5s. Exhaust with 11+ rapid attempts.
	rs := newTestReverseServer("node-1", "tok", false)

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws-node"

	var rateLimited int
	for i := 0; i < 15; i++ {
		conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			// HTTP 429 comes back as an HTTP error (WS upgrade failed).
			if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
				rateLimited++
			}
			continue
		}
		// Cleanly close accepted connections.
		conn.Close()
	}

	if rateLimited == 0 {
		t.Error("expected at least one 429 response from rate limiter")
	}
}

// ---- Display name capping ----

func TestReverseServer_DisplayNameCapping(t *testing.T) {
	auth := map[string]config.ReverseNodeEntry{
		"node-1": {Token: "tok"}, // no configured display name
	}
	rs := NewReverseServer(auth, false)

	connCh := make(chan *ReverseConn, 1)
	rs.OnRegister = func(id string, conn *ReverseConn) {
		select {
		case connCh <- conn:
		default:
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	defer conn.Close()

	longName := strings.Repeat("x", 400)
	conn.WriteJSON(ReverseMsg{
		Type:        "register",
		NodeID:      "node-1",
		Token:       "tok",
		DisplayName: longName,
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp ReverseMsg
	conn.ReadJSON(&resp)

	if resp.Type != "registered" {
		t.Fatalf("expected registered, got %q", resp.Type)
	}

	select {
	case rc := <-connCh:
		if len(rc.DisplayName()) > 256 {
			t.Errorf("display name should be capped at 256, got len=%d", len(rc.DisplayName()))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRegister callback")
	}
}

// ---- trustedProxy IP extraction for rate limiting ----

func TestReverseServer_TrustedProxy_rateLimit(t *testing.T) {
	// With trustedProxy=true, XFF last entry is used as rate-limit key.
	// Verify server starts without panic and accepts connections.
	rs := newTestReverseServer("node-1", "tok", true)

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Dial without XFF — falls back to RemoteAddr.
	conn := dialReverseNode(t, srv)
	defer conn.Close()
	resp := reverseAuth(t, conn, "node-1", "tok", "host")
	if resp.Type != "registered" {
		t.Fatalf("expected registered with trustedProxy, got %q", resp.Type)
	}
}
