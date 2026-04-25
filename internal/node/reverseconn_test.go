package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/config"
)

// setupReverseConnPair creates a ReverseServer + test HTTP server and dials in
// a "node" WebSocket client. Returns the server side ReverseConn (via
// OnRegister) and the client-side *websocket.Conn (for the test to control).
func setupReverseConnPair(t *testing.T) (*ReverseConn, *websocket.Conn, func()) {
	t.Helper()

	rs := NewReverseServer(map[string]config.ReverseNodeEntry{
		"worker": {Token: "tok", DisplayName: "Worker"},
	}, false)

	connCh := make(chan *ReverseConn, 1)
	rs.OnRegister = func(id string, rc *ReverseConn) { connCh <- rc }

	mux := http.NewServeMux()
	mux.Handle("/ws-node", rs)
	srv := httptest.NewServer(mux)

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws-node"
	wsConn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial /ws-node: %v", err)
	}

	// Register.
	wsConn.WriteJSON(ReverseMsg{Type: "register", NodeID: "worker", Token: "tok", Hostname: "host"})
	wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var regResp ReverseMsg
	if err := wsConn.ReadJSON(&regResp); err != nil || regResp.Type != "registered" {
		wsConn.Close()
		srv.Close()
		t.Fatalf("register failed: %v (type=%q)", err, regResp.Type)
	}
	wsConn.SetReadDeadline(time.Time{})

	var rc *ReverseConn
	select {
	case rc = <-connCh:
	case <-time.After(3 * time.Second):
		wsConn.Close()
		srv.Close()
		t.Fatal("timeout waiting for OnRegister")
	}

	cleanup := func() {
		wsConn.Close()
		rc.Close()
		srv.Close()
	}
	return rc, wsConn, cleanup
}

// ---- Accessors ----

func TestReverseConn_Accessors(t *testing.T) {
	rc, _, cleanup := setupReverseConnPair(t)
	defer cleanup()

	if rc.NodeID() != "worker" {
		t.Errorf("NodeID: want 'worker', got %q", rc.NodeID())
	}
	if rc.DisplayName() != "Worker" {
		t.Errorf("DisplayName: want 'Worker', got %q", rc.DisplayName())
	}
	if rc.Status() != "ok" {
		t.Errorf("Status: want 'ok', got %q", rc.Status())
	}
	if rc.RemoteAddr() == "" {
		t.Error("RemoteAddr should not be empty")
	}
}

// ---- FetchSessions via RPC ----

func TestReverseConn_FetchSessions(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	// Simulate the remote node handling the RPC.
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal([]map[string]any{{"session_id": "s1"}})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sessions, err := rc.FetchSessions(ctx)
	if err != nil {
		t.Fatalf("FetchSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
}

// ---- FetchEvents via RPC ----

func TestReverseConn_FetchEvents(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal([]cli.EventEntry{{Time: 1000, Type: "text"}})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	entries, err := rc.FetchEvents(ctx, "key", 0)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

// ---- Send via RPC ----

func TestReverseConn_Send(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := rc.Send(ctx, "key", "hello", ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// ---- RPC returns error from remote ----

func TestReverseConn_RPC_remoteError(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Error: "not found"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := rc.FetchSessions(ctx)
	if err == nil {
		t.Fatal("expected error from remote")
	}
}

// ---- RPC context cancellation cleans up pending ----

func TestReverseConn_RPC_contextCancelled(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	// Server side: don't respond.
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var req ReverseMsg
		wsConn.ReadJSON(&req) //nolint:errcheck
		// No response — let context expire.
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := rc.FetchSessions(ctx)
	if err == nil {
		t.Fatal("expected error on context cancellation")
	}

	// Pending map should be empty.
	rc.pendingMu.Lock()
	n := len(rc.pending)
	rc.pendingMu.Unlock()
	if n != 0 {
		t.Errorf("expected empty pending map after cancellation, got %d entries", n)
	}
}

// ---- RPC on closed connection returns error ----

func TestReverseConn_RPC_closedConnection(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	// Close the remote end.
	wsConn.Close()
	time.Sleep(50 * time.Millisecond) // let readLoop detect disconnect

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := rc.FetchSessions(ctx)
	if err == nil {
		t.Fatal("expected error when node is disconnected")
	}
}

// ---- readLoop delivers event to subscriber ----

func TestReverseConn_ReadLoop_event(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	event := &cli.EventEntry{Time: 1234, Type: "text", Summary: "hello"}
	wsConn.WriteJSON(ReverseMsg{Type: "event", Key: "mykey", Event: event})

	// Wait for delivery.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.RawMsgCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rawMsgs := sink.RawMsgs()
	if len(rawMsgs) == 0 {
		t.Fatal("expected event delivered to subscriber")
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs[0], &parsed); err != nil {
		t.Fatalf("invalid JSON delivered: %v", err)
	}
}

// ---- readLoop delivers subscribed message ----

func TestReverseConn_ReadLoop_subscribed(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	wsConn.WriteJSON(ReverseMsg{Type: "subscribed", Key: "mykey"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.RawMsgCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sink.RawMsgCount() == 0 {
		t.Fatal("expected subscribed message delivered")
	}
}

// ---- Subscribe sends subscribe message on wire ----

func TestReverseConn_Subscribe_sendsWireMessage(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.Subscribe(sink, "mykey", 500)

	wsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg ReverseMsg
	if err := wsConn.ReadJSON(&msg); err != nil {
		t.Fatalf("expected subscribe message on wire: %v", err)
	}
	if msg.Type != "subscribe" {
		t.Errorf("expected 'subscribe', got %q", msg.Type)
	}
	if msg.Key != "mykey" {
		t.Errorf("expected key 'mykey', got %q", msg.Key)
	}
}

// ---- Unsubscribe sends unsubscribed to sink ----

func TestReverseConn_Unsubscribe(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	go func() {
		// Drain the unsubscribe wire message.
		wsConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		var msg ReverseMsg
		wsConn.ReadJSON(&msg) //nolint:errcheck
	}()

	rc.Unsubscribe(sink, "mykey")

	found := false
	for _, m := range sink.JSONMsgs() {
		if msg, ok := m.(ServerMsg); ok && msg.Type == "unsubscribed" && msg.Key == "mykey" {
			found = true
		}
	}
	if !found {
		t.Error("expected unsubscribed message in sink")
	}
}

// ---- RefreshSubscription sends subscribe when subscribers exist ----

func TestReverseConn_RefreshSubscription(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	msgCh := make(chan ReverseMsg, 2)
	go func() {
		for {
			wsConn.SetReadDeadline(time.Now().Add(1 * time.Second))
			var msg ReverseMsg
			if err := wsConn.ReadJSON(&msg); err != nil {
				return
			}
			msgCh <- msg
		}
	}()

	rc.RefreshSubscription("mykey")

	select {
	case msg := <-msgCh:
		if msg.Type != "subscribe" || msg.Key != "mykey" {
			t.Errorf("expected subscribe for mykey, got type=%q key=%q", msg.Type, msg.Key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for subscribe message")
	}
}

// ---- RefreshSubscription no-op when no subscribers ----

func TestReverseConn_RefreshSubscription_noSubs(t *testing.T) {
	rc, _, cleanup := setupReverseConnPair(t)
	defer cleanup()
	// Should not panic; no wire message sent.
	rc.RefreshSubscription("no-such-key")
}

// ---- RemoveClient removes all subscriptions ----

func TestReverseConn_RemoveClient(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink1 := &mockSink{id: 1}
	sink2 := &mockSink{id: 2}

	rc.subMu.Lock()
	rc.subs["key1"] = []EventSink{sink1, sink2}
	rc.subs["key2"] = []EventSink{sink1}
	rc.subMu.Unlock()

	// Drain wire messages.
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			var msg ReverseMsg
			if err := wsConn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}()

	rc.RemoveClient(sink1)

	rc.subMu.Lock()
	for _, s := range rc.subs["key1"] {
		if s == sink1 {
			t.Error("sink1 should have been removed from key1")
		}
	}
	if _, ok := rc.subs["key2"]; ok {
		t.Error("key2 should have been removed since only sink1 was there")
	}
	rc.subMu.Unlock()
}

// ---- Close is idempotent ----

func TestReverseConn_Close_idempotent(t *testing.T) {
	rc, _, cleanup := setupReverseConnPair(t)
	defer cleanup()
	rc.Close()
	rc.Close() // must not panic
}

// ---- ProxyTakeover ----

func TestReverseConn_ProxyTakeover(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal(map[string]string{"key": "feishu:group:42"})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	key, err := rc.ProxyTakeover(ctx, 99, "sess-x", "/cwd", 12345)
	if err != nil {
		t.Fatalf("ProxyTakeover: %v", err)
	}
	if key != "feishu:group:42" {
		t.Errorf("expected key 'feishu:group:42', got %q", key)
	}
}

// ---- ProxyCloseDiscovered ----

func TestReverseConn_ProxyCloseDiscovered(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := rc.ProxyCloseDiscovered(ctx, 1, "s", "/", 0); err != nil {
		t.Fatalf("ProxyCloseDiscovered: %v", err)
	}
}

// ---- ProxyRestartPlanner ----

func TestReverseConn_ProxyRestartPlanner(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := rc.ProxyRestartPlanner(ctx, "my-proj"); err != nil {
		t.Fatalf("ProxyRestartPlanner: %v", err)
	}
}

// ---- ProxyUpdateConfig ----

func TestReverseConn_ProxyUpdateConfig(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := rc.ProxyUpdateConfig(ctx, "proj", json.RawMessage(`{"model":"c4"}`)); err != nil {
		t.Fatalf("ProxyUpdateConfig: %v", err)
	}
}

// ---- ProxyRemoveSession ----

func TestReverseConn_ProxyRemoveSession(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	var gotMethod string
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		gotMethod = req.Method
		result, _ := json.Marshal(map[string]bool{"removed": true})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	removed, err := rc.ProxyRemoveSession(ctx, "feishu:group:abc")
	if err != nil {
		t.Fatalf("ProxyRemoveSession: %v", err)
	}
	if !removed {
		t.Error("expected removed=true")
	}
	if gotMethod != "remove_session" {
		t.Errorf("expected method 'remove_session', got %q", gotMethod)
	}
}

func TestReverseConn_ProxyRemoveSession_notFound(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal(map[string]bool{"removed": false})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	removed, err := rc.ProxyRemoveSession(ctx, "k")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if removed {
		t.Error("expected removed=false")
	}
}

// ---- ProxyInterruptSession ----

func TestReverseConn_ProxyInterruptSession(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	var gotMethod string
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		gotMethod = req.Method
		result, _ := json.Marshal(map[string]bool{"interrupted": true})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	interrupted, err := rc.ProxyInterruptSession(ctx, "k")
	if err != nil {
		t.Fatalf("ProxyInterruptSession: %v", err)
	}
	if !interrupted {
		t.Error("expected interrupted=true")
	}
	if gotMethod != "interrupt_session" {
		t.Errorf("expected method 'interrupt_session', got %q", gotMethod)
	}
}

func TestReverseConn_ProxyInterruptSession_notRunning(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal(map[string]bool{"interrupted": false})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	interrupted, err := rc.ProxyInterruptSession(ctx, "k")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if interrupted {
		t.Error("expected interrupted=false")
	}
}

// ---- ProxySetSessionLabel ----

func TestReverseConn_ProxySetSessionLabel(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	var gotMethod string
	var gotParams map[string]string
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		gotMethod = req.Method
		_ = json.Unmarshal(req.Params, &gotParams)
		result, _ := json.Marshal(map[string]bool{"updated": true})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	updated, err := rc.ProxySetSessionLabel(ctx, "feishu:direct:alice:general", "my-label")
	if err != nil {
		t.Fatalf("ProxySetSessionLabel: %v", err)
	}
	if !updated {
		t.Error("expected updated=true")
	}
	if gotMethod != "set_session_label" {
		t.Errorf("method = %q, want set_session_label", gotMethod)
	}
	if gotParams["key"] != "feishu:direct:alice:general" {
		t.Errorf("params[key] = %q, want feishu:direct:alice:general", gotParams["key"])
	}
	if gotParams["label"] != "my-label" {
		t.Errorf("params[label] = %q, want my-label", gotParams["label"])
	}
}

func TestReverseConn_ProxySetSessionLabel_unknownKey(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal(map[string]bool{"updated": false})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	updated, err := rc.ProxySetSessionLabel(ctx, "k", "x")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if updated {
		t.Error("expected updated=false")
	}
}

// ---- FetchDiscoveredPreview ----

func TestReverseConn_FetchDiscoveredPreview(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal([]cli.EventEntry{{Time: 100, Type: "text"}})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	entries, err := rc.FetchDiscoveredPreview(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FetchDiscoveredPreview: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

// ---- FetchProjects ----

func TestReverseConn_FetchProjects(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal([]map[string]any{{"name": "proj1"}})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	projs, err := rc.FetchProjects(ctx)
	if err != nil {
		t.Fatalf("FetchProjects: %v", err)
	}
	if len(projs) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projs))
	}
}

// ---- FetchDiscovered ----

func TestReverseConn_FetchDiscovered(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		result, _ := json.Marshal([]map[string]any{{"pid": 100}})
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	disc, err := rc.FetchDiscovered(ctx)
	if err != nil {
		t.Fatalf("FetchDiscovered: %v", err)
	}
	if len(disc) != 1 {
		t.Fatalf("expected 1, got %d", len(disc))
	}
}

// ---- markDisconnected sets status to error ----

func TestReverseConn_MarkDisconnected_setsStatus(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	if rc.Status() != "ok" {
		t.Fatalf("initial status should be 'ok', got %q", rc.Status())
	}

	// Trigger disconnect by closing the ws connection.
	wsConn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rc.Status() == "error" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rc.Status() != "error" {
		t.Errorf("expected status 'error' after disconnect, got %q", rc.Status())
	}
}

// ---- subscribe_error removes key from subs ----

func TestReverseConn_ReadLoop_subscribeError(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.subs["badkey"] = []EventSink{sink}
	rc.subMu.Unlock()

	wsConn.WriteJSON(ReverseMsg{Type: "subscribe_error", Key: "badkey", Error: "session not found"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rc.subMu.Lock()
		_, exists := rc.subs["badkey"]
		rc.subMu.Unlock()
		if !exists {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	rc.subMu.Lock()
	_, exists := rc.subs["badkey"]
	rc.subMu.Unlock()
	if exists {
		t.Error("subscribe_error should have removed 'badkey' from subs")
	}

	if sink.RawMsgCount() == 0 {
		t.Error("expected error event delivered to sink")
	}
}

// TestReverseConn_EventsCappedOnPush locks down R67-SEC-3: a compromised
// reverse node that pushes a huge `events` array must not be able to fan
// that array out unbounded to every subscribed browser client. The cap is
// maxPushedHistoryEvents (500) and the broadcast should keep the tail so
// legitimate last-N history replays remain accurate.
func TestReverseConn_EventsCappedOnPush(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	// Build 800 events — well above the 500 cap. Each entry carries an
	// ascending Time so we can verify the tail (last 500) survives.
	events := make([]cli.EventEntry, 800)
	for i := range events {
		events[i] = cli.EventEntry{Time: int64(i + 1), Type: "text"}
	}
	wsConn.WriteJSON(ReverseMsg{Type: "events", Key: "mykey", Events: events})

	// Wait for delivery.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.RawMsgCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	msgs := sink.RawMsgs()
	if len(msgs) == 0 {
		t.Fatal("expected history message delivered to subscriber")
	}
	var parsed struct {
		Type   string           `json:"type"`
		Events []cli.EventEntry `json:"events"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Type != "history" {
		t.Errorf("type = %q, want 'history'", parsed.Type)
	}
	if got := len(parsed.Events); got != maxPushedHistoryEvents {
		t.Errorf("events length = %d, want %d (capped)", got, maxPushedHistoryEvents)
	}
	// Verify tail preserved: the last event's Time must be 800 (index 799 + 1).
	if n := len(parsed.Events); n > 0 {
		if got := parsed.Events[n-1].Time; got != 800 {
			t.Errorf("tail event Time = %d, want 800 (tail should be preserved)", got)
		}
		if got := parsed.Events[0].Time; got != int64(800-maxPushedHistoryEvents+1) {
			t.Errorf("head-after-cap Time = %d, want %d", got, 800-maxPushedHistoryEvents+1)
		}
	}
}

// TestReverseConn_EventsUnderCapPassesThrough verifies that normal-sized
// history replays (<= cap) are not truncated — the cap is defense against
// abuse, not a legitimate limit.
func TestReverseConn_EventsUnderCapPassesThrough(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	events := make([]cli.EventEntry, 100)
	for i := range events {
		events[i] = cli.EventEntry{Time: int64(i + 1), Type: "text"}
	}
	wsConn.WriteJSON(ReverseMsg{Type: "events", Key: "mykey", Events: events})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.RawMsgCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	msgs := sink.RawMsgs()
	if len(msgs) == 0 {
		t.Fatal("expected history message delivered to subscriber")
	}
	var parsed struct {
		Events []cli.EventEntry `json:"events"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := len(parsed.Events); got != 100 {
		t.Errorf("events length = %d, want 100 (no truncation expected)", got)
	}
}

// TestTruncateLabelUTF8 verifies R67-SEC-6: byte-level truncation at a
// multi-byte rune boundary preserves UTF-8 validity by stripping the
// trailing partial-rune fragment.
func TestTruncateLabelUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under cap passes unchanged", "hello", 10, "hello"},
		{"exactly at cap passes unchanged", "hello", 5, "hello"},
		{"ASCII cleanly cut", "helloworld", 5, "hello"},
		// 中 = E4 B8 AD (3 bytes). Cutting at byte 4 lands mid-rune: "中" + partial "国" (E5 9B ..).
		{"multibyte preserved when cap covers complete runes", "中国", 6, "中国"},
		{"multibyte cut mid-rune drops partial rune", "中国", 4, "中"},
		{"multibyte cut mid-rune drops to empty when first rune truncated", "中国", 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateLabelUTF8(c.in, c.max)
			if got != c.want {
				t.Errorf("truncateLabelUTF8(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("result %q contains invalid UTF-8", got)
			}
		})
	}
}
