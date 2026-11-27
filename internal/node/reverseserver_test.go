package node

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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
	auth := map[string]ReverseNodeAuth{
		nodeID: {Token: token, DisplayName: "Test Node"},
	}
	return NewReverseServer(auth, trustedProxy)
}

// ---- Happy-path registration ----

func TestReverseServer_Register_ok(t *testing.T) {
	rs := newTestReverseServer("node-1", "secret", false)

	registered := make(chan string, 1)
	rs.OnRegister = func(id string, conn *ReverseConn) { registered <- id }

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

	// OnRegister fires on the server goroutine; join deterministically.
	select {
	case id := <-registered:
		if id != "node-1" {
			t.Errorf("OnRegister id = %q, want node-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("OnRegister was not called")
	}
}

// ---- Bad credentials are rejected ----

// A node configured with an empty token cannot authenticate even with an
// empty probe token (the zero-value collision the precomputed-hash fast path
// must not allow, #2231).
func TestReverseServer_Register_rejected(t *testing.T) {
	rs := NewReverseServer(map[string]ReverseNodeAuth{
		"node-1":     {Token: "correct"},
		"node-empty": {Token: ""},
	}, false)

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, tc := range []struct{ name, nodeID, token string }{
		{"wrong token", "node-1", "wrong"},
		{"unknown node_id", "unknown-node", "correct"},
		{"empty configured token", "node-empty", ""},
	} {
		conn := dialReverseNode(t, srv)
		if resp := reverseAuth(t, conn, tc.nodeID, tc.token, "host"); resp.Type != "register_fail" {
			t.Errorf("%s: expected 'register_fail', got %q", tc.name, resp.Type)
		}
		conn.Close()
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

// ---- AllNodes returns configured nodes, connected or not ----

func TestReverseServer_AllNodes(t *testing.T) {
	auth := map[string]ReverseNodeAuth{
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

// ---- Reconnect with same node_id closes old connection ----

func TestReverseServer_Reconnect_closesOldConn(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	// The registered ack is written before the server re-checks who owns the
	// node id, so a conn that gets displaced in that window has its OnRegister
	// suppressed by design. Carrying the *ReverseConn and waiting for conn1's
	// own registration before dialing conn2 keeps the sequence a fact of the
	// test rather than of the scheduler.
	registered := make(chan *ReverseConn, 4)
	rs.OnRegister = func(id string, conn *ReverseConn) { registered <- conn }

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn1 := dialReverseNode(t, srv)
	resp1 := reverseAuth(t, conn1, "node-1", "tok", "host")
	if resp1.Type != "registered" {
		t.Fatalf("conn1: expected registered, got %q", resp1.Type)
	}
	var rc1 *ReverseConn
	select {
	case rc1 = <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("conn1: OnRegister never fired while it was the only conn for node-1")
	}

	conn2 := dialReverseNode(t, srv)
	resp2 := reverseAuth(t, conn2, "node-1", "tok", "host")
	if resp2.Type != "registered" {
		t.Fatalf("conn2: expected registered, got %q", resp2.Type)
	}
	defer conn2.Close()

	// conn1 must be closed by the server; drain any non-close frame still in
	// flight and only accept a real close (a read deadline means it never
	// happened).
	conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := conn1.ReadMessage()
		if err == nil {
			continue
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Error("conn1 was not closed after reconnect with same node_id (read timed out)")
		}
		break
	}

	// The reconnecting conn takes over the id, so its OnRegister must fire and
	// must carry a different *ReverseConn than the one it displaced — that is
	// what keeps downstream maps pointing at the live socket.
	select {
	case rc2 := <-registered:
		if rc2 == rc1 {
			t.Error("conn2's OnRegister carried the displaced conn; downstream maps would keep writing to the closed socket")
		}
	case <-time.After(2 * time.Second):
		t.Error("conn2 took over node-1 but its OnRegister never fired")
	}
	select {
	case <-registered:
		t.Error("OnRegister fired a third time for two connections")
	default:
	}
}

// ---- Stale deregister of a replaced conn must NOT fire OnDeregister ----
//
// R20260605B-CORR-14: when a node reconnects under the same node_id, the
// registration handler closes the OLD conn and installs the NEW one, then
// calls OnRegister(new). The old conn's deregister goroutine later unblocks
// and previously called OnDeregister(node_id) UNCONDITIONALLY — which, in
// server.go, deletes the live node from Server.nodes and purges its cache,
// making the freshly reconnected node invisible. The fix gates OnDeregister
// with the same ownership check (s.conns[id] == rc) used for the map delete,
// so a stale deregister of a replaced conn is a no-op. OnDeregister must
// fire exactly once, only when the LIVE conn finally drops.
func TestReverseServer_StaleDeregister_doesNotFireForReplacedConn(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)
	rs.deregisterGrace = testDeregisterGrace

	deregistered := make(chan string, 4)
	rs.OnDeregister = func(id string) { deregistered <- id }
	// The server writes the "registered" frame BEFORE installing the conn
	// into s.conns, so the client-side ack does not prove the server-side
	// map insert happened. If conn1's handler is descheduled in that gap
	// while conn2 registers, conn2 lands first and conn1 then overwrites
	// it — conn2 becomes an orphan whose close never deregisters (owns ==
	// false) and conn1 is never closed. OnRegister fires only after the
	// insert, so gate each dial on it to pin the order this test assumes.
	registered := make(chan struct{}, 4)
	rs.OnRegister = func(string, *ReverseConn) { registered <- struct{}{} }
	waitRegistered := func(label string) {
		select {
		case <-registered:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: timeout waiting for OnRegister", label)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn1 := dialReverseNode(t, srv)
	if resp := reverseAuth(t, conn1, "node-1", "tok", "host"); resp.Type != "registered" {
		t.Fatalf("conn1: expected registered, got %q", resp.Type)
	}
	waitRegistered("conn1")

	conn2 := dialReverseNode(t, srv)
	if resp := reverseAuth(t, conn2, "node-1", "tok", "host"); resp.Type != "registered" {
		t.Fatalf("conn2: expected registered, got %q", resp.Type)
	}
	waitRegistered("conn2")

	// conn1 is closed by the server on reconnect; its deregister goroutine
	// unblocks. The stale deregister must NOT fire because conn2 (the live
	// conn) now owns s.conns["node-1"].
	conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, _ = conn1.ReadMessage() // drain the server-side close

	select {
	case id := <-deregistered:
		t.Fatalf("stale OnDeregister fired for replaced conn (id=%q); live node would be wiped", id)
	case <-time.After(300 * time.Millisecond):
		// Good: no deregister while conn2 is live.
	}

	// Now drop the LIVE conn — OnDeregister must fire exactly once.
	conn2.Close()
	select {
	case id := <-deregistered:
		if id != "node-1" {
			t.Errorf("expected deregister for node-1, got %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for OnDeregister after live conn dropped")
	}

	// Ensure no spurious second deregister arrives.
	select {
	case id := <-deregistered:
		t.Fatalf("unexpected extra OnDeregister for %q", id)
	case <-time.After(200 * time.Millisecond):
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
	auth := map[string]ReverseNodeAuth{
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

// ---- Behind a TLS-terminating proxy (CloudFront → ALB → naozhi) ----

// The proxy hop arrives as plain HTTP with the public Host, so only a declared
// trusted proxy's X-Forwarded-Proto: https lets the upgrade through.
func TestReverseServer_Upgrade_forwardedHTTPS(t *testing.T) {
	for _, tc := range []struct {
		name         string
		trustedProxy bool
		wantUpgrade  bool
	}{
		{"trusted proxy", true, true},
		{"no trusted proxy", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newTestReverseServer("node-1", "tok", tc.trustedProxy)
			mux := http.NewServeMux()
			mux.Handle("/ws-node", rs)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws-node"
			hdr := http.Header{"Host": {"naozhi.example.com"}, "X-Forwarded-Proto": {"https"}}
			conn, resp, err := websocket.DefaultDialer.Dial(u, hdr)
			if conn != nil {
				defer conn.Close()
			}
			if got := err == nil; got != tc.wantUpgrade {
				status := 0
				if resp != nil {
					status = resp.StatusCode
				}
				t.Fatalf("upgrade = %v (status %d, err %v), want %v", got, status, err, tc.wantUpgrade)
			}
			// Behind a trusted proxy the rate-limit key comes from XFF, falling
			// back to RemoteAddr; registration goes through either way.
			if conn != nil {
				if r := reverseAuth(t, conn, "node-1", "tok", "host"); r.Type != "registered" {
					t.Fatalf("expected registered with trustedProxy, got %q", r.Type)
				}
			}
		})
	}
}
