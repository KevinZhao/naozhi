package node

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// currentConn returns the conn registered for id, or nil.
func (s *ReverseServer) currentConn(id string) *ReverseConn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conns[id]
}

// waitConnGone polls until no conn is registered for id.
func waitConnGone(t *testing.T, rs *ReverseServer, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rs.currentConn(id) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("conn for %q still registered after rollback", id)
}

// expectClosedByServer reads one frame and requires a non-timeout error,
// i.e. the server actively closed the socket.
func expectClosedByServer(t *testing.T, conn *websocket.Conn, label string) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("%s: expected server-side close, got a frame", label)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("%s: server never closed the conn (read timed out)", label)
	}
}

// ---- #2458: fast reconnect during the insert→ack window ----
//
// conn1 reaches the insert point and is parked (test hook) before its ack is
// written. conn2 then completes a full handshake under the same node_id.
// Required outcome: conn2 owns the registry entry, conn1 is closed by the
// server, and dropping conn2 fires OnDeregister. Under the old order (ack
// before insert) conn1 overwrote conn2, conn1 was never closed, and conn2's
// close never deregistered.
func TestReverseServer_FastReconnectDuringAck_newConnWins(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	registered := make(chan *ReverseConn, 4)
	rs.OnRegister = func(_ string, rc *ReverseConn) { registered <- rc }
	deregistered := make(chan string, 4)
	rs.OnDeregister = func(id string) { deregistered <- id }

	release := make(chan struct{})
	parked := make(chan struct{})
	var hookCalls atomic.Int32
	rs.testHookBeforeAck = func(*ReverseConn) {
		if hookCalls.Add(1) == 1 {
			close(parked)
			<-release
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn1 := dialReverseNode(t, srv)
	defer conn1.Close()
	if err := conn1.WriteJSON(ReverseMsg{Type: "register", NodeID: "node-1", Token: "tok", Hostname: "h"}); err != nil {
		t.Fatalf("conn1 register: %v", err)
	}
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("conn1 handler never reached the ack hook")
	}

	conn2 := dialReverseNode(t, srv)
	defer conn2.Close()
	if resp := reverseAuth(t, conn2, "node-1", "tok", "h"); resp.Type != "registered" {
		t.Fatalf("conn2: expected registered, got %q", resp.Type)
	}
	var rc2 *ReverseConn
	select {
	case rc2 = <-registered:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for OnRegister(conn2)")
	}

	close(release)

	// conn1 must have been closed by the server (displaced by conn2).
	expectClosedByServer(t, conn1, "conn1")

	if got := rs.currentConn("node-1"); got != rc2 {
		t.Fatalf("registry must point at conn2 (%p), got %p", rc2, got)
	}
	// No stale OnRegister for the displaced conn1.
	select {
	case rc := <-registered:
		t.Fatalf("unexpected OnRegister for displaced conn %p", rc)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case id := <-deregistered:
		t.Fatalf("unexpected OnDeregister(%q) while conn2 is live", id)
	default:
	}

	// Dropping the live conn2 must deregister exactly once.
	conn2.Close()
	select {
	case id := <-deregistered:
		if id != "node-1" {
			t.Errorf("expected deregister for node-1, got %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("conn2 close did not fire OnDeregister (orphaned conn)")
	}
	select {
	case id := <-deregistered:
		t.Fatalf("unexpected extra OnDeregister for %q", id)
	case <-time.After(200 * time.Millisecond):
	}
}

// ---- #2458: ack write failure rolls the insert back (no prior conn) ----
func TestReverseServer_AckWriteFails_rollsBackInsert(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	var registerCalls atomic.Int32
	rs.OnRegister = func(string, *ReverseConn) { registerCalls.Add(1) }
	deregistered := make(chan string, 4)
	rs.OnDeregister = func(id string) { deregistered <- id }
	// Close the freshly installed conn so the ack write fails.
	rs.testHookBeforeAck = func(rc *ReverseConn) { rc.Close() }

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn := dialReverseNode(t, srv)
	defer conn.Close()
	if err := conn.WriteJSON(ReverseMsg{Type: "register", NodeID: "node-1", Token: "tok", Hostname: "h"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	expectClosedByServer(t, conn, "conn")
	waitConnGone(t, rs, "node-1")

	if n := registerCalls.Load(); n != 0 {
		t.Fatalf("OnRegister must not fire for an un-acked conn, got %d", n)
	}
	select {
	case id := <-deregistered:
		t.Fatalf("no conn was displaced, but OnDeregister(%q) fired", id)
	case <-time.After(200 * time.Millisecond):
	}
}

// ---- #2458: ack failure after displacing a live conn owes OnDeregister ----
//
// conn1 is registered (OnRegister fired). conn2 displaces it (conn1 closed,
// its own deregister suppressed by the identity check) and then fails its
// ack. The rollback must remove conn2 from the map AND fire OnDeregister on
// behalf of the displaced conn1 so the upper layer does not keep a dead
// conn registered.
func TestReverseServer_AckWriteFails_afterDisplace_firesOnDeregister(t *testing.T) {
	rs := newTestReverseServer("node-1", "tok", false)

	registered := make(chan struct{}, 4)
	rs.OnRegister = func(string, *ReverseConn) { registered <- struct{}{} }
	deregistered := make(chan string, 4)
	rs.OnDeregister = func(id string) { deregistered <- id }

	var hookCalls atomic.Int32
	rs.testHookBeforeAck = func(rc *ReverseConn) {
		if hookCalls.Add(1) == 2 {
			rc.Close() // second registration (conn2) fails its ack
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	conn1 := dialReverseNode(t, srv)
	defer conn1.Close()
	if resp := reverseAuth(t, conn1, "node-1", "tok", "h"); resp.Type != "registered" {
		t.Fatalf("conn1: expected registered, got %q", resp.Type)
	}
	select {
	case <-registered:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for OnRegister(conn1)")
	}

	conn2 := dialReverseNode(t, srv)
	defer conn2.Close()
	if err := conn2.WriteJSON(ReverseMsg{Type: "register", NodeID: "node-1", Token: "tok", Hostname: "h"}); err != nil {
		t.Fatalf("conn2 register: %v", err)
	}
	expectClosedByServer(t, conn2, "conn2")
	expectClosedByServer(t, conn1, "conn1")
	waitConnGone(t, rs, "node-1")

	select {
	case id := <-deregistered:
		if id != "node-1" {
			t.Errorf("expected deregister for node-1, got %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rollback after displacing conn1 did not fire OnDeregister; dead conn left registered upstream")
	}
	select {
	case id := <-deregistered:
		t.Fatalf("unexpected extra OnDeregister for %q", id)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-registered:
		t.Fatal("unexpected OnRegister for conn2 whose ack failed")
	default:
	}
}
