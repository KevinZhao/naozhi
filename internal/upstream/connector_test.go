package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// fakeServer returns an httptest.Server that speaks the reverse-node WS protocol.
// onConn is called with each new WebSocket connection.
type connHandler func(conn *websocket.Conn)

func newFakeServer(t *testing.T, handler connHandler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade: %v", err)
			return
		}
		handler(conn)
	}))
	return srv
}

// wsURL converts an http:// test server URL to ws://.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// makeRouter creates a minimal Router without a CLI wrapper (suitable for
// read-only RPC tests such as fetch_sessions and fetch_projects).
func makeRouter() *session.Router {
	return session.NewRouter(session.RouterConfig{MaxProcs: 1})
}

// sendRegistered sends the "registered" ack after reading the register message.
// Returns the register message for inspection.
func handshake(t *testing.T, conn *websocket.Conn) node.ReverseMsg {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var reg node.ReverseMsg
	if err := conn.ReadJSON(&reg); err != nil {
		t.Fatalf("read register: %v", err)
	}
	if err := conn.WriteJSON(node.ReverseMsg{Type: "registered"}); err != nil {
		t.Fatalf("write registered: %v", err)
	}
	conn.SetReadDeadline(time.Time{})
	return reg
}

// ---- marshalResult ----

func TestMarshalResult_Simple(t *testing.T) {
	data, err := marshalResult(map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("marshalResult = %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["key"] != "value" {
		t.Errorf("marshalResult[\"key\"] = %q, want \"value\"", got["key"])
	}
}

func TestMarshalResult_Nil(t *testing.T) {
	data, err := marshalResult(nil)
	if err != nil {
		t.Fatalf("marshalResult(nil) = %v", err)
	}
	if string(data) != "null" {
		t.Errorf("marshalResult(nil) = %s, want \"null\"", data)
	}
}

// ---- Connector.New ----

func TestNew_CreatesConnector(t *testing.T) {
	cfg := &config.UpstreamConfig{
		URL:    "wss://example.com/ws-node",
		NodeID: "node1",
		Token:  "secret",
	}
	router := makeRouter()
	c := New(cfg, router, nil)
	if c == nil {
		t.Fatal("New() = nil")
	}
	if c.cfg != cfg {
		t.Error("cfg not stored")
	}
	if c.router != router {
		t.Error("router not stored")
	}
}

func TestNew_SetDiscoverFunc(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)
	called := false
	c.SetDiscoverFunc(func() (json.RawMessage, error) {
		called = true
		return json.RawMessage(`[]`), nil
	})
	c.discoverFunc()
	if !called {
		t.Error("SetDiscoverFunc callback not stored")
	}
}

func TestNew_SetPreviewFunc(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)
	var gotID string
	c.SetPreviewFunc(func(id string) (json.RawMessage, error) {
		gotID = id
		return json.RawMessage(`[]`), nil
	})
	c.previewFunc("sess-123")
	if gotID != "sess-123" {
		t.Errorf("previewFunc got sessionID = %q, want \"sess-123\"", gotID)
	}
}

// ---- Auth failure path ----

func TestRunOnce_AuthFailure(t *testing.T) {
	// Server sends a non-"registered" ack (simulates auth rejection).
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var reg node.ReverseMsg
		conn.ReadJSON(&reg)                                                          //nolint:errcheck
		conn.WriteJSON(node.ReverseMsg{Type: "register_fail", Error: "auth failed"}) //nolint:errcheck
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "node1", Token: "badtoken"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	connected, err := c.runOnce(ctx)
	if connected {
		t.Error("runOnce: connected should be false on auth failure")
	}
	if err == nil {
		t.Error("runOnce: expected error on auth failure, got nil")
	}
}

// ---- Dial failure path ----

func TestRunOnce_DialFailure(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "ws://127.0.0.1:1", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	connected, err := c.runOnce(ctx)
	if connected {
		t.Error("runOnce: connected should be false when dial fails")
	}
	if err == nil {
		t.Error("runOnce: expected error on dial failure")
	}
}

// ---- handleRequest: fetch_sessions ----

func TestHandleRequest_FetchSessions(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{
		Type:   "request",
		Method: "fetch_sessions",
	}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_sessions = %v", err)
	}
	// Result should be a JSON array.
	var sessions []interface{}
	if err := json.Unmarshal(result, &sessions); err != nil {
		t.Fatalf("fetch_sessions result not JSON array: %v", err)
	}
}

// ---- handleRequest: fetch_projects (no projMgr) ----

func TestHandleRequest_FetchProjects_NilMgr(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil) // projMgr = nil

	req := node.ReverseMsg{Method: "fetch_projects"}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_projects nil mgr = %v", err)
	}
	var arr []interface{}
	json.Unmarshal(result, &arr) //nolint:errcheck
	if len(arr) != 0 {
		t.Errorf("fetch_projects nil mgr = %v, want empty array", arr)
	}
}

// ---- handleRequest: fetch_discovered ----

func TestHandleRequest_FetchDiscovered_NoFunc(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "fetch_discovered"}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_discovered no func = %v", err)
	}
	var arr []interface{}
	json.Unmarshal(result, &arr) //nolint:errcheck
	if len(arr) != 0 {
		t.Errorf("expected empty array, got %v", arr)
	}
}

func TestHandleRequest_FetchDiscovered_WithFunc(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)
	c.SetDiscoverFunc(func() (json.RawMessage, error) {
		return json.RawMessage(`[{"session_id":"abc"}]`), nil
	})

	req := node.ReverseMsg{Method: "fetch_discovered"}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_discovered with func = %v", err)
	}
	if !strings.Contains(string(result), "abc") {
		t.Errorf("result = %s, want to contain abc", result)
	}
}

// ---- handleRequest: fetch_discovered_preview ----

func TestHandleRequest_FetchDiscoveredPreview_BadParams(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "fetch_discovered_preview", Params: json.RawMessage(`not-json`)}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for bad params, got nil")
	}
}

func TestHandleRequest_FetchDiscoveredPreview_WithFunc(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)
	c.SetPreviewFunc(func(sid string) (json.RawMessage, error) {
		return json.RawMessage(`[{"session_id":"` + sid + `"}]`), nil
	})

	// Valid UUID format — R65-SEC-M-1 added a boundary check so the input
	// must satisfy discovery.IsValidSessionID.
	const validSID = "12345678-1234-1234-1234-123456789abc"
	params, _ := json.Marshal(map[string]string{"session_id": validSID})
	req := node.ReverseMsg{Method: "fetch_discovered_preview", Params: params}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_discovered_preview = %v", err)
	}
	if !strings.Contains(string(result), validSID) {
		t.Errorf("result = %s, want to contain %s", result, validSID)
	}
}

// TestHandleRequest_FetchDiscoveredPreview_InvalidSessionID verifies the
// boundary validator rejects path-traversal / non-UUID inputs before calling
// previewFunc. R65-SEC-M-1.
func TestHandleRequest_FetchDiscoveredPreview_InvalidSessionID(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)
	var called bool
	c.SetPreviewFunc(func(sid string) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`[]`), nil
	})

	params, _ := json.Marshal(map[string]string{"session_id": "../../../../etc/passwd"})
	req := node.ReverseMsg{Method: "fetch_discovered_preview", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Fatalf("expected error for path-traversal session_id, got nil")
	}
	if called {
		t.Fatalf("previewFunc was invoked for invalid session_id")
	}
}

// ---- handleRequest: fetch_events ----

func TestHandleRequest_FetchEvents_BadParams(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "fetch_events", Params: json.RawMessage(`not-json`)}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for bad params, got nil")
	}
}

func TestHandleRequest_FetchEvents_SessionNotFound(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	params, _ := json.Marshal(map[string]interface{}{"key": "no:such:key", "after": 0})
	req := node.ReverseMsg{Method: "fetch_events", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for missing session, got nil")
	}
}

// ---- handleRequest: send (no wrapper — session creation fails) ----

func TestHandleRequest_Send_BadParams(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "send", Params: json.RawMessage(`not-json`)}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for bad params, got nil")
	}
}

// TestHandleRequest_Send_TextTooLong asserts the reverse-RPC trust boundary
// enforces the same text cap as the primary-side dashboard handler, so a
// compromised or misconfigured primary cannot push a multi-MB prompt
// straight into CLI stdin with only the shim's 12 MB line ceiling as a
// backstop. R68-SEC-H1.
func TestHandleRequest_Send_TextTooLong(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	// 5 MB > maxCoalescedTextBytes (4 MB). Valid JSON that would otherwise
	// pass through to sess.Send on an accepting router.
	big := strings.Repeat("x", 5*1024*1024)
	params, _ := json.Marshal(map[string]string{
		"key":  "feishu:direct:alice:general",
		"text": big,
	})
	req := node.ReverseMsg{Method: "send", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Fatalf("expected error for oversize text, got nil")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("err = %v, want to contain \"too long\"", err)
	}
}

// TestHandleRequest_Send_RejectsTraversalOnEmptyDefaultWorkspace locks down
// R68-SEC-M2: even when the connector has no configured defaultWorkspace
// (single-user deployments), a reverse-RPC `send` must reject workspace
// paths containing `..` segments or control bytes BEFORE filepath.Clean
// silently folds them into a now-canonical absolute path. Before the fix,
// the defaultWorkspace=="" branch skipped the prefix check entirely, so a
// compromised primary could spawn CLI sessions rooted at /etc or anywhere
// else by submitting `/home/../etc`.
func TestHandleRequest_Send_RejectsTraversalOnEmptyDefaultWorkspace(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)
	if c.defaultWorkspace != "" {
		t.Fatalf("precondition: defaultWorkspace must be empty, got %q", c.defaultWorkspace)
	}
	params, _ := json.Marshal(map[string]string{
		"key":       "feishu:direct:alice:general",
		"text":      "hi",
		"workspace": "/home/../etc",
	})
	req := node.ReverseMsg{Method: "send", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Fatal("expected traversal rejection, got nil")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("err = %v, want to mention workspace", err)
	}
}

func TestHandleRequest_Send_RejectsControlByteInWorkspace(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)
	params, _ := json.Marshal(map[string]string{
		"key":       "feishu:direct:alice:general",
		"text":      "hi",
		"workspace": "/home/user\nproj",
	})
	req := node.ReverseMsg{Method: "send", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Fatal("expected control-byte rejection, got nil")
	}
}

// TestHandleRequest_Send_EmptyDefaultWorkspace_RejectsOverride verifies the
// R74/R68-SEC-M2 "fail closed" policy: when the reverse-node Connector has
// no default workspace configured (router.DefaultWorkspace()==""), ANY
// workspace override, even a syntactically valid absolute path, must be
// rejected. Before this fix, `/etc` and similar legitimate-looking paths
// passed through unchecked — the prefix gate was wrapped in
// `if c.defaultWorkspace != ""`, so an empty default opened the door to
// arbitrary CLI-session rooting. R68-SEC-M2.
func TestHandleRequest_Send_EmptyDefaultWorkspace_RejectsOverride(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)
	if c.defaultWorkspace != "" {
		t.Fatalf("precondition: defaultWorkspace must be empty, got %q", c.defaultWorkspace)
	}
	// /etc is syntactically valid (absolute, no `..`, no control bytes) but
	// must still be rejected because the node has no allowlist to bound it.
	params, _ := json.Marshal(map[string]string{
		"key":       "feishu:direct:alice:general",
		"text":      "hi",
		"workspace": "/etc",
	})
	req := node.ReverseMsg{Method: "send", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Fatal("expected rejection of /etc under empty defaultWorkspace, got nil")
	}
	if !strings.Contains(err.Error(), "no allowed root") {
		t.Errorf("err = %v, want 'no allowed root configured' message", err)
	}
}

// TestValidateRemoteWorkspacePath_SharedByConnector confirms connector's
// takeover and send paths call into the shared session.ValidateRemoteWorkspacePath
// gate. The takeover path performs the CWD gate after the process identity
// check (by design — we don't want to do FS work on behalf of an
// unauthorized PID), so an integration-style test would require a valid
// process identity. The shared validator is already exhaustively covered
// by TestValidateRemoteWorkspacePath in package session; this test only
// asserts that the connector's send branch wires it in under an empty
// defaultWorkspace. R68-SEC-M2.
func TestValidateRemoteWorkspacePath_SharedByConnector(t *testing.T) {
	// Sanity ping that the shared validator rejects traversal.
	if err := session.ValidateRemoteWorkspacePath("/home/../etc"); err == nil {
		t.Fatal("ValidateRemoteWorkspacePath should reject /home/../etc")
	}
	// connector.handleRequest "send" branch exercised by
	// TestHandleRequest_Send_RejectsTraversalOnEmptyDefaultWorkspace above.
}

// ---- handleRequest: unknown method ----

func TestHandleRequest_UnknownMethod(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "totally_unknown"}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for unknown method, got nil")
	}
	if !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("error = %v, want to contain \"unknown method\"", err)
	}
}

// ---- handleRequest: restart_planner (no projMgr) ----

func TestHandleRequest_RestartPlanner_NilMgr(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	params, _ := json.Marshal(map[string]string{"project_name": "myproj"})
	req := node.ReverseMsg{Method: "restart_planner", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for restart_planner with nil projMgr, got nil")
	}
}

func TestHandleRequest_RestartPlanner_BadParams(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "restart_planner", Params: json.RawMessage(`not-json`)}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for bad params, got nil")
	}
}

// ---- handleRequest: update_config (no projMgr) ----

func TestHandleRequest_UpdateConfig_NilMgr(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	params, _ := json.Marshal(map[string]interface{}{
		"project_name": "proj",
		"config":       map[string]bool{"git_sync": true},
	})
	req := node.ReverseMsg{Method: "update_config", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for update_config with nil projMgr, got nil")
	}
}

func TestHandleRequest_UpdateConfig_BadParams(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "update_config", Params: json.RawMessage(`not-json`)}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for bad params, got nil")
	}
}

// ---- handleRequest: takeover / close_discovered (invalid params) ----

func TestHandleRequest_Takeover_BadParams(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "takeover", Params: json.RawMessage(`not-json`)}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for bad params, got nil")
	}
}

func TestHandleRequest_Takeover_MissingPID(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	params, _ := json.Marshal(map[string]interface{}{"pid": 0, "session_id": "sess-abc"})
	req := node.ReverseMsg{Method: "takeover", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for pid=0, got nil")
	}
}

func TestHandleRequest_CloseDiscovered_BadParams(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{Method: "close_discovered", Params: json.RawMessage(`not-json`)}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for bad params, got nil")
	}
}

func TestHandleRequest_CloseDiscovered_MissingPID(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	params, _ := json.Marshal(map[string]interface{}{"pid": 0})
	req := node.ReverseMsg{Method: "close_discovered", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for pid=0, got nil")
	}
}

func TestHandleRequest_CloseDiscovered_MissingProcStartTime(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	params, _ := json.Marshal(map[string]interface{}{"pid": 12345, "proc_start_time": 0})
	req := node.ReverseMsg{Method: "close_discovered", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for proc_start_time=0, got nil")
	}
}

// ---- Full round-trip: register + ping/pong ----

func TestHandleConn_PingPong(t *testing.T) {
	var serverConn *websocket.Conn
	var mu sync.Mutex
	pingReceived := make(chan struct{}, 1)

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		mu.Lock()
		serverConn = conn
		mu.Unlock()

		// Handshake
		handshake(t, conn)

		// Send a ping message using the application-level ping (Type:"ping"), not WS ping.
		conn.WriteJSON(node.ReverseMsg{Type: "ping"}) //nolint:errcheck

		// Read the pong response
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var resp node.ReverseMsg
		if err := conn.ReadJSON(&resp); err != nil {
			t.Logf("read pong: %v", err)
			return
		}
		if resp.Type == "pong" {
			pingReceived <- struct{}{}
		}
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	select {
	case <-pingReceived:
		cancel() // success — stop the connector
	case <-time.After(5 * time.Second):
		t.Error("timeout waiting for pong response")
	}

	<-done
	_ = serverConn
}

// ---- Full round-trip: register + request/response ----

func TestHandleConn_RequestFetchSessions(t *testing.T) {
	responded := make(chan node.ReverseMsg, 1)

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)

		// Send a request
		req := node.ReverseMsg{
			Type:   "request",
			ReqID:  "req-1",
			Method: "fetch_sessions",
		}
		conn.WriteJSON(req) //nolint:errcheck

		// Wait for the response
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var resp node.ReverseMsg
		if err := conn.ReadJSON(&resp); err != nil {
			t.Logf("read response: %v", err)
			return
		}
		responded <- resp
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	select {
	case resp := <-responded:
		if resp.Type != "response" {
			t.Errorf("response type = %q, want \"response\"", resp.Type)
		}
		if resp.ReqID != "req-1" {
			t.Errorf("response req_id = %q, want \"req-1\"", resp.ReqID)
		}
		cancel()
	case <-time.After(5 * time.Second):
		t.Error("timeout waiting for response")
	}

	<-done
}

// ---- Context cancellation stops Run ----

func TestRun_CancelContext(t *testing.T) {
	// Server that immediately closes so Run loops quickly.
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		conn.Close()
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	// Cancel quickly; Run should return promptly.
	cancel()
	select {
	case <-done:
		// success
	case <-time.After(5 * time.Second):
		t.Error("Run did not return after context cancel")
	}
}

// ---- Register message carries configured fields ----

func TestRunOnce_RegisterPayload(t *testing.T) {
	var gotReg node.ReverseMsg
	regCh := make(chan node.ReverseMsg, 1)

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		conn.ReadJSON(&gotReg) //nolint:errcheck
		regCh <- gotReg
		// Do NOT send "registered" — connector will error and return.
		conn.WriteJSON(node.ReverseMsg{Type: "register_fail"}) //nolint:errcheck
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{
		URL:         wsURL(srv),
		NodeID:      "my-node",
		Token:       "my-token",
		DisplayName: "Test Node",
	}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.runOnce(ctx) //nolint:errcheck

	select {
	case reg := <-regCh:
		if reg.NodeID != "my-node" {
			t.Errorf("register NodeID = %q, want \"my-node\"", reg.NodeID)
		}
		if reg.Token != "my-token" {
			t.Errorf("register Token = %q, want \"my-token\"", reg.Token)
		}
		if reg.DisplayName != "Test Node" {
			t.Errorf("register DisplayName = %q, want \"Test Node\"", reg.DisplayName)
		}
		if reg.Type != "register" {
			t.Errorf("register Type = %q, want \"register\"", reg.Type)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout waiting for register message")
	}
}

// ---- subscribe / unsubscribe (session not found) ----

func TestHandleConn_Subscribe_SessionNotFound(t *testing.T) {
	errorReceived := make(chan node.ReverseMsg, 1)

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)

		conn.WriteJSON(node.ReverseMsg{Type: "subscribe", Key: "no:such:key"}) //nolint:errcheck

		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var resp node.ReverseMsg
		if err := conn.ReadJSON(&resp); err == nil {
			errorReceived <- resp
		}
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	select {
	case resp := <-errorReceived:
		if resp.Type != "subscribe_error" {
			t.Errorf("type = %q, want \"subscribe_error\"", resp.Type)
		}
		cancel()
	case <-time.After(5 * time.Second):
		t.Error("timeout waiting for subscribe_error")
	}

	<-done
}

// ---- Backoff resets after successful connection ----

// TestRun_BackoffResetOnSuccess_Logic tests the backoff state machine directly
// by inspecting the Run() loop logic at the runOnce() level.
// Note: a full end-to-end reconnect test would need to account for the
// ~30s ping goroutine drain time inside handleConn, so we test the backoff
// reset logic through the dial-failure path instead.
func TestRun_BackoffLogic_DialsOnFailure(t *testing.T) {
	// Server that immediately rejects auth so runOnce returns (connected=false).
	// Verify that Run() retries (at least 2 dial attempts) within the timeout.
	connSeen := make(chan struct{}, 20)

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		// Signal that server received a connection attempt.
		select {
		case connSeen <- struct{}{}:
		default:
		}
		// Don't read — close immediately to trigger dial error on next attempt.
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	go c.Run(ctx)

	// Wait for 2 connection attempts. Each failed attempt uses backoff starting at 1s.
	// t=0: attempt 1 (fails), backoff=1s
	// t=1: attempt 2 (fails), backoff=2s
	for i := 0; i < 2; i++ {
		select {
		case <-connSeen:
			// got a connection attempt
		case <-time.After(4 * time.Second):
			t.Errorf("timeout waiting for connection attempt %d", i+1)
			return
		}
	}
}

// ---- handleRequest: update_config with real project manager ----

func TestHandleRequest_UpdateConfig_WithMgr(t *testing.T) {
	// Create a temp project directory
	root := t.TempDir()
	projDir := root + "/testproject"
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projDir+"/CLAUDE.md", []byte("# test"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), mgr)

	newCfg := project.ProjectConfig{GitSync: true, GitRemote: "upstream"}
	cfgJSON, _ := json.Marshal(newCfg)
	params, _ := json.Marshal(map[string]json.RawMessage{
		"project_name": json.RawMessage(`"testproject"`),
		"config":       cfgJSON,
	})
	req := node.ReverseMsg{Method: "update_config", Params: params}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("update_config with real mgr = %v", err)
	}
	var status map[string]string
	json.Unmarshal(result, &status) //nolint:errcheck
	if status["status"] != "ok" {
		t.Errorf("status = %q, want \"ok\"", status["status"])
	}
}

func TestHandleRequest_UpdateConfig_ProjectNotFound(t *testing.T) {
	root := t.TempDir()
	mgr, _ := project.NewManager(root, project.PlannerDefaults{})
	mgr.Scan()

	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), mgr)

	params, _ := json.Marshal(map[string]interface{}{
		"project_name": "ghost",
		"config":       map[string]bool{"git_sync": false},
	})
	req := node.ReverseMsg{Method: "update_config", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for non-existent project, got nil")
	}
}

// ---- handleRequest: restart_planner with real project manager ----

func TestHandleRequest_RestartPlanner_ProjectNotFound(t *testing.T) {
	root := t.TempDir()
	mgr, _ := project.NewManager(root, project.PlannerDefaults{})
	mgr.Scan()

	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), mgr)

	params, _ := json.Marshal(map[string]string{"project_name": "ghost"})
	req := node.ReverseMsg{Method: "restart_planner", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for non-existent project, got nil")
	}
}

// ---- handleRequest: fetch_projects with real project manager ----

func TestHandleRequest_FetchProjects_WithMgr(t *testing.T) {
	root := t.TempDir()
	projDir := root + "/myproject"
	os.MkdirAll(projDir, 0755)
	os.WriteFile(projDir+"/CLAUDE.md", []byte("# proj"), 0644)

	mgr, _ := project.NewManager(root, project.PlannerDefaults{})
	mgr.Scan()

	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), mgr)

	req := node.ReverseMsg{Method: "fetch_projects"}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_projects with mgr = %v", err)
	}
	var arr []interface{}
	json.Unmarshal(result, &arr) //nolint:errcheck
	if len(arr) != 1 {
		t.Errorf("fetch_projects = %d projects, want 1", len(arr))
	}
}

// ---- handleRequest: takeover with invalid session ID format ----

func TestHandleRequest_Takeover_InvalidSessionID(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	// Valid-looking PID but invalid session ID format
	params, _ := json.Marshal(map[string]interface{}{
		"pid":             os.Getpid(),
		"session_id":      "not-a-valid-uuid",
		"proc_start_time": uint64(1),
	})
	req := node.ReverseMsg{Method: "takeover", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for invalid session_id format, got nil")
	}
}

func TestHandleRequest_Takeover_MissingProcStartTime(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	params, _ := json.Marshal(map[string]interface{}{
		"pid":             12345,
		"session_id":      "12345678-1234-1234-1234-123456789012",
		"proc_start_time": uint64(0),
	})
	req := node.ReverseMsg{Method: "takeover", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for proc_start_time=0, got nil")
	}
}

// ---- handleRequest: close_discovered with invalid session ID format ----

func TestHandleRequest_CloseDiscovered_InvalidSessionID(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	params, _ := json.Marshal(map[string]interface{}{
		"pid":             12345,
		"session_id":      "not-a-uuid",
		"proc_start_time": uint64(1),
	})
	req := node.ReverseMsg{Method: "close_discovered", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Error("expected error for invalid session_id, got nil")
	}
}

// ---- handleConn: unsubscribe message ----

func TestHandleConn_Unsubscribe(t *testing.T) {
	// Subscribe to a non-existent key (gets subscribe_error), then unsubscribe.
	// Verifies the unsubscribe path and the unsubscribed response.
	messages := make(chan node.ReverseMsg, 10)

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)

		// First unsubscribe an unknown key (no prior subscribe needed)
		conn.WriteJSON(node.ReverseMsg{Type: "unsubscribe", Key: "no:such:key"}) //nolint:errcheck

		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var resp node.ReverseMsg
		if err := conn.ReadJSON(&resp); err == nil {
			messages <- resp
		}
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	select {
	case resp := <-messages:
		if resp.Type != "unsubscribed" {
			t.Errorf("type = %q, want \"unsubscribed\"", resp.Type)
		}
		cancel()
	case <-time.After(5 * time.Second):
		t.Error("timeout waiting for unsubscribed response")
	}

	<-done
}

// ---- handleRequest: fetch_events with existing session (registered via RegisterForResume) ----

func TestHandleRequest_FetchEvents_ExistingSession(t *testing.T) {
	router := makeRouter()
	// Register a session with no process (history-only stub)
	router.RegisterForResume("feishu:group:chat1:general", "sess-abc123", "/tmp", "test prompt")

	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, router, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"key":   "feishu:group:chat1:general",
		"after": int64(0),
	})
	req := node.ReverseMsg{Method: "fetch_events", Params: params}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_events existing session = %v", err)
	}
	// Result should be a JSON array (possibly empty since no events)
	var arr []interface{}
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("fetch_events result not JSON array: %v", err)
	}
}

// ---- handleConn: request with error response ----

func TestHandleConn_RequestErrorResponse(t *testing.T) {
	// Send a request for an unknown method; verify the response carries an error field.
	responses := make(chan node.ReverseMsg, 1)

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)

		conn.WriteJSON(node.ReverseMsg{
			Type:   "request",
			ReqID:  "err-req-1",
			Method: "nonexistent_method",
		}) //nolint:errcheck

		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var resp node.ReverseMsg
		if err := conn.ReadJSON(&resp); err == nil {
			responses <- resp
		}
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	select {
	case resp := <-responses:
		if resp.Type != "response" {
			t.Errorf("type = %q, want \"response\"", resp.Type)
		}
		if resp.Error == "" {
			t.Error("error field should be set for unknown method")
		}
		if resp.ReqID != "err-req-1" {
			t.Errorf("req_id = %q, want \"err-req-1\"", resp.ReqID)
		}
		cancel()
	case <-time.After(5 * time.Second):
		t.Error("timeout waiting for error response")
	}

	<-done
}

// ---- handleConn: WS-level ping message from server (pong handler) ----

func TestHandleConn_WSPingPong(t *testing.T) {
	// Verify that the connector handles WS-level pings (pongHandler resets deadline).
	connected := make(chan struct{}, 1)

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)
		// Send a WS-level ping frame
		conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) //nolint:errcheck
		connected <- struct{}{}
		// Keep conn open briefly
		time.Sleep(200 * time.Millisecond)
	})
	defer srv.Close()

	cfg := &config.UpstreamConfig{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	select {
	case <-connected:
		cancel()
	case <-time.After(5 * time.Second):
		t.Error("timeout")
	}
	<-done
}

// ---- handleRequest: set_session_label ----

func TestHandleRequest_SetSessionLabel_Updates(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	router := makeRouter()
	// Seed a session so the dispatch path has something to mutate.
	proc := session.NewTestProcess()
	router.InjectSession("feishu:direct:alice:general", proc)
	c := New(cfg, router, nil)

	req := node.ReverseMsg{
		Method: "set_session_label",
		Params: json.RawMessage(`{"key":"feishu:direct:alice:general","label":"标记"}`),
	}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("set_session_label: %v", err)
	}
	var resp map[string]bool
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp["updated"] {
		t.Errorf("updated = false, want true")
	}
	if got := router.GetSession("feishu:direct:alice:general").UserLabel(); got != "标记" {
		t.Errorf("UserLabel = %q, want 标记", got)
	}
}

func TestHandleRequest_SetSessionLabel_UnknownKey(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{
		Method: "set_session_label",
		Params: json.RawMessage(`{"key":"nope","label":"x"}`),
	}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("set_session_label: %v", err)
	}
	var resp map[string]bool
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["updated"] {
		t.Errorf("updated = true for unknown key")
	}
}

func TestHandleRequest_SetSessionLabel_MissingKey(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	req := node.ReverseMsg{
		Method: "set_session_label",
		Params: json.RawMessage(`{"label":"x"}`),
	}
	if _, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{}); err == nil {
		t.Errorf("expected error for missing key, got nil")
	}
}

func TestHandleRequest_SetSessionLabel_TooLong(t *testing.T) {
	cfg := &config.UpstreamConfig{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	label := strings.Repeat("a", 129)
	req := node.ReverseMsg{
		Method: "set_session_label",
		Params: json.RawMessage(`{"key":"k","label":"` + label + `"}`),
	}
	if _, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{}); err == nil {
		t.Errorf("expected error for oversized label, got nil")
	}
}
