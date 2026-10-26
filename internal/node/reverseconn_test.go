package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// setupReverseConnPair creates a ReverseServer + test HTTP server and dials in
// a "node" WebSocket client. Returns the server side ReverseConn (via
// OnRegister) and the client-side *websocket.Conn (for the test to control).
func setupReverseConnPair(t *testing.T) (*ReverseConn, *websocket.Conn, func()) {
	t.Helper()

	rs := NewReverseServer(map[string]ReverseNodeAuth{
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

// ---- Per-RPC wire method, params and result ----

// TestReverseConn_RPCs checks, for every method, the wire method name and
// params it sends and how it decodes the node's result.
func TestReverseConn_RPCs(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	for _, tc := range []struct {
		name   string
		method string
		params map[string]any // nil: no params
		result string         // raw JSON result; "" sends none
		call   func(context.Context) (any, error)
		want   any
	}{
		{
			name: "FetchSessions", method: "fetch_sessions",
			result: `[{"session_id":"s1"}]`,
			call:   func(ctx context.Context) (any, error) { s, err := rc.FetchSessions(ctx); return len(s), err },
			want:   1,
		},
		{
			name: "FetchEvents", method: "fetch_events",
			params: map[string]any{"key": "key", "after": 0.0},
			result: `[{"time":1000,"type":"text"}]`,
			call:   func(ctx context.Context) (any, error) { e, err := rc.FetchEvents(ctx, "key", 0); return len(e), err },
			want:   1,
		},
		{
			// A response with no result is a node that took the send.
			name: "Send", method: "send",
			params: map[string]any{"key": "key", "text": "hello"},
			call:   func(ctx context.Context) (any, error) { return rc.Send(ctx, "key", "hello", "") },
			want:   "accepted",
		},
		{
			name: "ProxyTakeover", method: "takeover",
			params: map[string]any{"pid": 99.0, "session_id": "sess-x", "cwd": "/cwd", "proc_start_time": 12345.0},
			result: `{"key":"feishu:group:42"}`,
			call:   func(ctx context.Context) (any, error) { return rc.ProxyTakeover(ctx, 99, "sess-x", "/cwd", 12345) },
			want:   "feishu:group:42",
		},
		{
			name: "ProxyCloseDiscovered", method: "close_discovered",
			params: map[string]any{"pid": 1.0, "session_id": "s", "cwd": "/", "proc_start_time": 0.0},
			call:   func(ctx context.Context) (any, error) { return nil, rc.ProxyCloseDiscovered(ctx, 1, "s", "/", 0) },
		},
		{
			name: "ProxyUpdateConfig", method: "update_config",
			params: map[string]any{"project_name": "proj", "config": map[string]any{"model": "c4"}},
			call: func(ctx context.Context) (any, error) {
				return nil, rc.ProxyUpdateConfig(ctx, "proj", json.RawMessage(`{"model":"c4"}`))
			},
		},
		{
			name: "ProxyRemoveSession", method: "remove_session",
			params: map[string]any{"key": "feishu:group:abc"},
			result: `{"removed":true}`,
			call:   func(ctx context.Context) (any, error) { return rc.ProxyRemoveSession(ctx, "feishu:group:abc") },
			want:   true,
		},
		{
			name: "ProxyRemoveSession not found", method: "remove_session",
			params: map[string]any{"key": "k"},
			result: `{"removed":false}`,
			call:   func(ctx context.Context) (any, error) { return rc.ProxyRemoveSession(ctx, "k") },
			want:   false,
		},
		{
			name: "ProxyInterruptSession", method: "interrupt_session",
			params: map[string]any{"key": "k"},
			result: `{"interrupted":true}`,
			call:   func(ctx context.Context) (any, error) { return rc.ProxyInterruptSession(ctx, "k") },
			want:   true,
		},
		{
			name: "ProxyInterruptSession not running", method: "interrupt_session",
			params: map[string]any{"key": "k"},
			result: `{"interrupted":false}`,
			call:   func(ctx context.Context) (any, error) { return rc.ProxyInterruptSession(ctx, "k") },
			want:   false,
		},
		{
			name: "ProxySetSessionLabel", method: "set_session_label",
			params: map[string]any{"key": "feishu:direct:alice:general", "label": "my-label"},
			result: `{"updated":true}`,
			call: func(ctx context.Context) (any, error) {
				return rc.ProxySetSessionLabel(ctx, "feishu:direct:alice:general", "my-label")
			},
			want: true,
		},
		{
			name: "ProxySetSessionLabel unknown key", method: "set_session_label",
			params: map[string]any{"key": "k", "label": "x"},
			result: `{"updated":false}`,
			call:   func(ctx context.Context) (any, error) { return rc.ProxySetSessionLabel(ctx, "k", "x") },
			want:   false,
		},
		{
			name: "FetchDiscoveredPreview", method: "fetch_discovered_preview",
			params: map[string]any{"session_id": "sess-1"},
			result: `[{"time":100,"type":"text"}]`,
			call: func(ctx context.Context) (any, error) {
				e, err := rc.FetchDiscoveredPreview(ctx, "sess-1")
				return len(e), err
			},
			want: 1,
		},
		{
			name: "FetchProjects", method: "fetch_projects",
			result: `[{"name":"proj1"}]`,
			call:   func(ctx context.Context) (any, error) { p, err := rc.FetchProjects(ctx); return len(p), err },
			want:   1,
		},
		{
			name: "FetchDiscovered", method: "fetch_discovered",
			result: `[{"pid":100}]`,
			call:   func(ctx context.Context) (any, error) { d, err := rc.FetchDiscovered(ctx); return len(d), err },
			want:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan ReverseMsg, 1)
			go func() {
				wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
				var req ReverseMsg
				if err := wsConn.ReadJSON(&req); err != nil {
					close(seen)
					return
				}
				resp := ReverseMsg{Type: "response", ReqID: req.ReqID}
				if tc.result != "" {
					resp.Result = json.RawMessage(tc.result)
				}
				wsConn.WriteJSON(resp)
				seen <- req // after the write, so the next row's goroutine never overlaps it
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			got, err := tc.call(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("result = %#v, want %#v", got, tc.want)
			}
			req, ok := <-seen
			if !ok {
				t.Fatal("node never read the request")
			}
			if req.Method != tc.method {
				t.Errorf("wire method = %q, want %q", req.Method, tc.method)
			}
			var params map[string]any
			if len(req.Params) > 0 {
				if err := json.Unmarshal(req.Params, &params); err != nil {
					t.Fatalf("params %s: %v", req.Params, err)
				}
			}
			if !reflect.DeepEqual(params, tc.params) {
				t.Errorf("params = %v, want %v", params, tc.params)
			}
		})
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
	testhelper.Eventually(t, func() bool { return rc.Status() == "error" }, 2*time.Second, "readLoop never observed the closed conn")

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
	rc.book.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	event := &clievent.EventEntry{Time: 1234, Type: "text", Summary: "hello"}
	wsConn.WriteJSON(ReverseMsg{Type: "event", Key: "mykey", Event: event})

	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() > 0 }, 2*time.Second, "expected event delivered to subscriber")

	rawMsgs := sink.RawMsgs()
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
	rc.book.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	wsConn.WriteJSON(ReverseMsg{Type: "subscribed", Key: "mykey"})

	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() > 0 }, 2*time.Second, "expected subscribed message delivered")
}

// ---- Subscribe sends subscribe message on wire ----

func TestReverseConn_Subscribe_sendsWireMessage(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.Subscribe(sink, "mykey", 500, 0)

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
	rc.book.subs["mykey"] = []EventSink{sink}
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
	rc.book.subs["mykey"] = []EventSink{sink}
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
	rc.book.subs["key1"] = []EventSink{sink1, sink2}
	rc.book.subs["key2"] = []EventSink{sink1}
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
	for _, s := range rc.book.subs["key1"] {
		if s == sink1 {
			t.Error("sink1 should have been removed from key1")
		}
	}
	if _, ok := rc.book.subs["key2"]; ok {
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

// ---- markDisconnected sets status to error ----

func TestReverseConn_MarkDisconnected_setsStatus(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	if rc.Status() != "ok" {
		t.Fatalf("initial status should be 'ok', got %q", rc.Status())
	}

	// Trigger disconnect by closing the ws connection.
	wsConn.Close()

	testhelper.Eventually(t, func() bool { return rc.Status() == "error" }, 3*time.Second, "expected status \"error\" after disconnect")
}

// ---- subscribe_error removes key from subs ----

func TestReverseConn_ReadLoop_subscribeError(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.book.subs["badkey"] = []EventSink{sink}
	rc.subMu.Unlock()

	wsConn.WriteJSON(ReverseMsg{Type: "subscribe_error", Key: "badkey", Error: "session not found"})

	// Poll on delivery, NOT on the key disappearing. broadcastToSubs deletes
	// the key while holding subMu but calls SendRaw *after* releasing it, so
	// "key gone" is observable while RawMsgCount() is still 0 — polling the
	// key raced the assertion below and made this test flaky. Delivery is the
	// strictly later signal, so waiting for it settles both observations.
	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() > 0 }, 2*time.Second, "expected error event delivered to sink")

	rc.subMu.Lock()
	_, exists := rc.book.subs["badkey"]
	rc.subMu.Unlock()
	if exists {
		t.Error("subscribe_error should have removed 'badkey' from subs")
	}
}

// TestReverseConn_SubscribeErrorAfterSubscribed_KeepsSinks: a node that acked
// a key and then answers a refresh with subscribe_error (its session was
// reset) leaves the browsers on the key, so the next session's events reach
// them.
func TestReverseConn_SubscribeErrorAfterSubscribed_KeepsSinks(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.book.subs["k"] = []EventSink{sink}
	rc.subMu.Unlock()

	wsConn.WriteJSON(ReverseMsg{Type: "subscribed", Key: "k"})
	wsConn.WriteJSON(ReverseMsg{Type: "subscribe_error", Key: "k", Error: "session not found"})
	wsConn.WriteJSON(ReverseMsg{Type: "event", Key: "k", Event: &clievent.EventEntry{Time: 1, Type: "text", Summary: "next"}})

	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() == 3 }, 2*time.Second,
		"the event after the subscribe_error never reached the sink")
	if !strings.Contains(string(sink.RawMsgs()[1]), "session not found") {
		t.Errorf("second frame = %s, want the subscribe_error passed on", sink.RawMsgs()[1])
	}
}

// rawGateSink blocks inside SendRaw until released, so a test can park the
// readLoop in the window between broadcastToSubs deleting the sub key (under
// subMu) and the actual fan-out to sinks (after subMu is released).
type rawGateSink struct {
	mockSink
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newRawGateSink() *rawGateSink {
	return &rawGateSink{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (s *rawGateSink) SendRaw(data []byte) {
	s.once.Do(func() {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	})
	<-s.gate
	s.mockSink.SendRaw(data)
}

// TestReverseConn_SubscribeError_KeyRemovalPrecedesDelivery pins the ordering
// that made TestReverseConn_ReadLoop_subscribeError flaky: broadcastToSubs
// deletes the key while holding subMu but delivers to sinks only after
// releasing it. A test that polls "key gone" as a proxy for "event delivered"
// can therefore break out of its wait loop and assert delivery too early.
//
// Parking inside SendRaw makes that window deterministic instead of a
// microsecond race, so a future refactor that reintroduces the key-polling
// pattern (or moves delivery back under the lock) gets a red test here rather
// than a rare CI flake.
func TestReverseConn_SubscribeError_KeyRemovalPrecedesDelivery(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	sink := newRawGateSink()
	rc.subMu.Lock()
	rc.book.subs["badkey"] = []EventSink{sink}
	rc.subMu.Unlock()

	wsConn.WriteJSON(ReverseMsg{Type: "subscribe_error", Key: "badkey", Error: "session not found"})

	select {
	case <-sink.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("SendRaw never reached; subscribe_error was not broadcast")
	}

	// Parked inside SendRaw: the key is already gone, but nothing has been
	// delivered yet. This is exactly the state the old poll loop could
	// observe before asserting RawMsgCount() != 0.
	rc.subMu.Lock()
	_, exists := rc.book.subs["badkey"]
	rc.subMu.Unlock()
	if exists {
		t.Error("key should already be removed once SendRaw is reached")
	}
	if n := sink.RawMsgCount(); n != 0 {
		t.Errorf("delivery recorded before SendRaw completed: RawMsgCount = %d, want 0", n)
	}

	close(sink.gate)

	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() > 0 }, 2*time.Second, "expected error event delivered to sink after gate release")
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
	rc.book.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	// Build 800 events — well above the 500 cap. Each entry carries an
	// ascending Time so we can verify the tail (last 500) survives.
	events := make([]clievent.EventEntry, 800)
	for i := range events {
		events[i] = clievent.EventEntry{Time: int64(i + 1), Type: "text"}
	}
	wsConn.WriteJSON(ReverseMsg{Type: "events", Key: "mykey", Events: events})

	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() > 0 }, 2*time.Second, "expected history message delivered to subscriber")

	msgs := sink.RawMsgs()
	var parsed struct {
		Type   string                `json:"type"`
		Events []clievent.EventEntry `json:"events"`
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
	rc.book.subs["mykey"] = []EventSink{sink}
	rc.subMu.Unlock()

	events := make([]clievent.EventEntry, 100)
	for i := range events {
		events[i] = clievent.EventEntry{Time: int64(i + 1), Type: "text"}
	}
	wsConn.WriteJSON(ReverseMsg{Type: "events", Key: "mykey", Events: events})

	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() > 0 }, 2*time.Second, "expected history message delivered to subscriber")

	msgs := sink.RawMsgs()
	var parsed struct {
		Events []clievent.EventEntry `json:"events"`
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

// TestReverseConn_RPCError_Sanitized verifies R20260607-SEC-13: a response
// error string from an inbound RPC message containing bidi/control injection
// sequences is sanitized before appearing in the returned error.
func TestReverseConn_RPCError_Sanitized(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	// Respond with an error field containing a bidi-override (U+202E).
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
		var req ReverseMsg
		if err := wsConn.ReadJSON(&req); err != nil {
			return
		}
		// Embed bidi-override in the error string.
		wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Error: "remote error: \u202einjected"}) //nolint:errcheck
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := rc.FetchSessions(ctx)
	if err == nil {
		t.Fatal("expected error from error response, got nil")
	}
	// The bidi-override (U+202E) must have been stripped by SanitizeForLog.
	if strings.ContainsRune(err.Error(), '‮') {
		t.Errorf("error still contains bidi-override rune: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "remote error") {
		t.Errorf("expected 'remote error' in message, got: %q", err.Error())
	}
}
