package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// wsTestServer spins up an httptest.Server with a gorilla WebSocket upgrader.
// onConn is called in a goroutine with the accepted connection.
func wsTestServer(t *testing.T, onConn func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		onConn(conn)
	}))
	return srv
}

// newRelayNode builds an HTTPClient pointing at srv and creates a wsRelay.
func newRelayNode(srv *httptest.Server) *HTTPClient {
	// wsURL → ws:// strip "/ws" from the URL because newWSRelay appends "/ws"
	// We need the node.URL to be the server base (without "/ws").
	return NewHTTPClient("test-node", srv.URL, "tok", "Test Node")
}

// authHandshake performs the relay auth handshake on the server side:
// reads auth message, sends auth_ok.
func authHandshake(t *testing.T, conn *websocket.Conn) bool {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var msg ClientMsg
	if err := conn.ReadJSON(&msg); err != nil {
		t.Logf("server: failed to read auth: %v", err)
		return false
	}
	if msg.Type != "auth" {
		t.Logf("server: expected auth, got %q", msg.Type)
		return false
	}
	conn.SetReadDeadline(time.Time{})
	return conn.WriteJSON(ServerMsg{Type: "auth_ok"}) == nil
}

// ---- Subscribe triggers WS connection and subscribe message ----

func TestWSRelay_Subscribe_connectsAndSubscribes(t *testing.T) {
	var gotSubscribe atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	srv := wsTestServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var msg ClientMsg
		if err := conn.ReadJSON(&msg); err == nil && msg.Type == "subscribe" {
			gotSubscribe.Store(true)
		}
		wg.Done()
		// drain remaining to avoid broken pipe on server
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	sink := &mockSink{id: 1}
	relay.Subscribe(sink, "feishu:group:123", 0)

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for subscribe message")
	}

	if !gotSubscribe.Load() {
		t.Error("server did not receive subscribe message")
	}

	relay.Close()
}

// ---- Subscribe sends error event to sink when connection fails ----

func TestWSRelay_Subscribe_connectFailureSendsError(t *testing.T) {
	// Point to a port where nothing is listening.
	node := NewHTTPClient("bad-node", "http://127.0.0.1:1", "", "bad")
	relay := newWSRelay(node)

	sink := &mockSink{id: 1}
	relay.Subscribe(sink, "key", 0)

	msgs := sink.JSONMsgs()
	if len(msgs) == 0 {
		t.Fatal("expected error message sent to sink")
	}
	msg, ok := msgs[0].(ServerMsg)
	if !ok {
		t.Fatalf("expected ServerMsg, got %T", msgs[0])
	}
	if msg.Type != "error" {
		t.Errorf("expected type 'error', got %q", msg.Type)
	}
}

// ---- Unsubscribe sends unsubscribed message ----

func TestWSRelay_Unsubscribe_sendsUnsubscribed(t *testing.T) {
	srv := wsTestServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		// Drain all messages
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	sink := &mockSink{id: 1}
	relay.Subscribe(sink, "key1", 0) // dials and subscribes before returning
	relay.Unsubscribe(sink, "key1")

	found := false
	for _, m := range sink.JSONMsgs() {
		if msg, ok := m.(ServerMsg); ok && msg.Type == "unsubscribed" {
			found = true
		}
	}
	if !found {
		t.Error("expected unsubscribed message in sink")
	}
	relay.Close()
}

// ---- Close is idempotent ----

func TestWSRelay_Close_idempotent(t *testing.T) {
	node := NewHTTPClient("n", "http://127.0.0.1:1", "", "")
	relay := newWSRelay(node)
	relay.Close()
	relay.Close() // must not panic
}

// ---- Close during concurrent dial does not leak goroutine ----

func TestWSRelay_Close_duringDial_noLeak(t *testing.T) {
	// The server holds the auth reply until the test has called Close, so
	// Close lands while connect() is parked on the handshake.
	var upgradedOnce sync.Once
	upgraded := make(chan struct{})
	release := make(chan struct{})

	srv := wsTestServer(t, func(conn *websocket.Conn) {
		upgradedOnce.Do(func() { close(upgraded) })
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		if !authHandshake(t, conn) {
			conn.Close()
			return
		}
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	var wg sync.WaitGroup
	// Launch multiple concurrent subscribers to stress the connReady path.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink := &mockSink{}
			relay.Subscribe(sink, "key", 0)
		}()
	}

	select {
	case <-upgraded:
	case <-time.After(2 * time.Second):
		t.Fatal("no subscriber ever dialed")
	}
	relay.Close()
	close(release)
	wg.Wait()

	// The dial that finished after Close must not have published its conn
	// (and started readLoop/pingLoop on it).
	relay.mu.Lock()
	leaked := relay.conn != nil
	relay.mu.Unlock()
	if leaked {
		t.Error("a dial that completed after Close() installed its conn on the closed relay")
	}
	// After Close, ensureConnected must return "relay closed".
	if err := relay.ensureConnected(); err == nil || err.Error() != "relay closed" {
		t.Errorf("expected 'relay closed' error after Close(), got %v", err)
	}
}

// ---- RemoveClient removes subscriptions from all keys ----

func TestWSRelay_RemoveClient(t *testing.T) {
	srv := wsTestServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	sink1 := &mockSink{id: 1}
	sink2 := &mockSink{id: 2}

	// Subscribe registers the sink before it returns; no wait is needed.
	relay.Subscribe(sink1, "key1", 0)
	relay.Subscribe(sink2, "key1", 0)
	relay.Subscribe(sink1, "key2", 0)

	relay.RemoveClient(sink1)

	relay.mu.Lock()
	key1 := append([]EventSink(nil), relay.subs["key1"]...)
	_, key2Left := relay.subs["key2"]
	relay.mu.Unlock()

	for _, s := range key1 {
		if s == sink1 {
			t.Error("sink1 should have been removed from key1")
		}
	}
	if len(key1) != 1 || key1[0] != sink2 {
		t.Errorf("key1 subscribers = %v, want only sink2", key1)
	}
	if key2Left {
		t.Error("key2 lost its only subscriber but its entry is still there")
	}

	relay.Close()
}

// ---- readLoop delivers injected events to subscribers ----

func TestWSRelay_ReadLoop_deliversEvents(t *testing.T) {
	var receivedRaw [][]byte

	srv := wsTestServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		// Read the subscribe message.
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var sub ClientMsg
		if err := conn.ReadJSON(&sub); err != nil || sub.Type != "subscribe" {
			return
		}
		// Push an event.
		event := map[string]any{
			"type": "event",
			"key":  "feishu:group:123",
			"event": map[string]any{
				"time": 5000,
				"type": "text",
			},
		}
		conn.WriteJSON(event)
		// Hold the connection until the relay closes it.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	sink := &mockSink{id: 1}
	// rawMsgs is initialized empty by default

	relay.Subscribe(sink, "feishu:group:123", 0)

	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() > 0 }, 2*time.Second, "expected at least one event delivered to sink")

	msgs := sink.RawMsgs()

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("delivered message is not valid JSON: %v — got %q", err, msgs[0])
	}
	if _, ok := parsed["node"]; !ok {
		t.Errorf("expected 'node' field injected, got %q", msgs[0])
	}

	relay.Close()
	_ = receivedRaw
}

// ---- Second subscriber on same key gets history via HTTP, not re-subscribe ----

func TestWSRelay_SecondSubscriberGetsHistory(t *testing.T) {
	historyRequested := make(chan struct{}, 1)
	subscribeMsgCount := atomic.Int32{}
	sawBarrier := atomic.Bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			authHandshake(t, conn)
			conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			for {
				var msg ClientMsg
				if err := conn.ReadJSON(&msg); err != nil {
					return
				}
				if msg.Type == "subscribe" && msg.Key == "barrier" {
					sawBarrier.Store(true)
				} else if msg.Type == "subscribe" {
					subscribeMsgCount.Add(1)
				}
			}
		}
		if r.URL.Path == "/api/sessions/events" {
			historyRequested <- struct{}{}
			json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	sink1 := &mockSink{id: 1}
	sink2 := &mockSink{id: 2}

	relay.Subscribe(sink1, "key1", 0) // dials and writes the subscribe before returning
	relay.Subscribe(sink2, "key1", 0) // second subscriber — should NOT send subscribe to remote

	// Wait for history HTTP call.
	select {
	case <-historyRequested:
	case <-time.After(2 * time.Second):
		t.Fatal("expected history HTTP request for second subscriber")
	}

	// A first subscribe on another key travels the same ordered connection,
	// so once the server has read it, it has read anything sink2 sent.
	relay.Subscribe(&mockSink{id: 3}, "barrier", 0)
	testhelper.Eventually(t, sawBarrier.Load, 2*time.Second, "server never read the barrier subscribe")
	if n := subscribeMsgCount.Load(); n != 1 {
		t.Errorf("expected exactly 1 subscribe message to remote, got %d", n)
	}

	relay.Close()
}
