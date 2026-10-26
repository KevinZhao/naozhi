package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli"
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
	// Track every connection-handler goroutine so the test does not finish
	// while one is still running. A WebSocket connection is hijacked out of
	// net/http on Upgrade, so httptest.Server.Close() closes the listener but
	// does NOT wait for these goroutines — leaving a handler free to call
	// t.Logf (error-path diagnostics) after the test function returns, which
	// races tRunner's `t.done = true` write (testing.go:2023) and trips the
	// race detector. The t.Cleanup barrier below runs inside tRunner's defer
	// BEFORE t.done is set, so any handler t.Logf lands in the still-legal
	// cleanup window rather than racing completion.
	var wg sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count the goroutine from the FIRST line of ServeHTTP, before the
		// Upgrade — the failure branch's t.Logf must be inside the barrier
		// too, and ServeHTTP itself touches t via t.Helper/t.Logf. Adding
		// after Upgrade would leave the ServeHTTP→Upgrade window (and the
		// upgrade-error log) unsynchronized, which still races t.done.
		wg.Add(1)
		defer wg.Done()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade: %v", err)
			return
		}
		handler(conn)
	}))
	// Close (unblocks any read on a still-open hijacked conn — every handler
	// caps its reads with a deadline or exits on conn close, so none parks
	// forever) then wait for the handlers to drain. Registered here rather
	// than left to each caller's `defer srv.Close()` so the wg.Wait() barrier
	// is guaranteed to run; callers therefore drop their own defer srv.Close().
	t.Cleanup(func() {
		srv.Close()
		wg.Wait()
	})
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
	cfg := &Config{
		URL:    "wss://example.com/ws-node",
		NodeID: "node1",
		Token:  "secret",
	}
	router := testRouter(makeRouter())
	c := New(cfg, router, nil, Discovery{}, nil)
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

	cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "badtoken"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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
	cfg := &Config{URL: "ws://127.0.0.1:1", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil) // projMgr = nil

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
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{
		Sessions: func() (json.RawMessage, error) {
			return json.RawMessage(`[{"session_id":"abc"}]`), nil
		},
	}, nil)

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

func TestHandleRequest_FetchDiscoveredPreview_WithFunc(t *testing.T) {
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{
		Preview: func(sid string) (json.RawMessage, error) {
			return json.RawMessage(`[{"session_id":"` + sid + `"}]`), nil
		},
	}, nil)

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
// the preview func. R65-SEC-M-1.
func TestHandleRequest_FetchDiscoveredPreview_InvalidSessionID(t *testing.T) {
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)
	var called bool
	c.discovery.Preview = func(sid string) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`[]`), nil
	}

	params, _ := json.Marshal(map[string]string{"session_id": "../../../../etc/passwd"})
	req := node.ReverseMsg{Method: "fetch_discovered_preview", Params: params}
	_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err == nil {
		t.Fatalf("expected error for path-traversal session_id, got nil")
	}
	if called {
		t.Fatalf("the preview func was invoked for invalid session_id")
	}
}

// ---- handleRequest: fetch_backends ----

// TestHandleRequest_FetchBackends pins the reverse-RPC side of the picker
// node-aware fix: a "fetch_backends" request returns THIS node's manifest
// ({backends, default, detected}) so the primary's picker can render the
// remote node's backends + default. Uses a claude-wrapper router so the
// manifest carries a real backend + default to assert on.
func TestHandleRequest_FetchBackends(t *testing.T) {
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	w := cli.NewWrapperLazy("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude")
	w.CLIVersion = "2.1.100"
	router := session.NewRouter(session.RouterConfig{Wrapper: w})
	c := New(cfg, testRouter(router), nil, Discovery{}, nil)

	req := node.ReverseMsg{Type: "request", Method: "fetch_backends"}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_backends = %v", err)
	}
	var manifest struct {
		Default  string           `json:"default"`
		Backends []map[string]any `json:"backends"`
		Detected []map[string]any `json:"detected"`
	}
	if err := json.Unmarshal(result, &manifest); err != nil {
		t.Fatalf("fetch_backends result not a manifest object: %v", err)
	}
	if manifest.Default != "claude" {
		t.Errorf("default = %q, want claude", manifest.Default)
	}
	if len(manifest.Backends) != 1 || manifest.Backends[0]["id"] != "claude" {
		t.Errorf("backends = %+v, want [claude]", manifest.Backends)
	}
	// detected is coerced to a non-nil empty array so the frontend's
	// Array.isArray(detected) guard holds.
	if manifest.Detected == nil {
		t.Error("detected = nil, want non-null empty array for frontend guard")
	}
}

// ---- handleRequest: malformed or invalid params ----

// TestHandleRequest_RejectsBadInput covers the per-method parameter
// validation in handleRequest: each row must come back as an error (the
// send-specific rejections live in TestHandleRequest_Send_RejectsBeforeThePipeline).
func TestHandleRequest_RejectsBadInput(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	for _, tc := range []struct {
		name, method, params, wantErr string
	}{
		{name: "fetch_discovered_preview bad json", method: "fetch_discovered_preview", params: `not-json`},
		{name: "fetch_events bad json", method: "fetch_events", params: `not-json`},
		{name: "fetch_events unknown session", method: "fetch_events", params: `{"key":"no:such:key","after":0}`},
		{name: "send bad json", method: "send", params: `not-json`},
		{name: "unknown method", method: "totally_unknown", wantErr: "unknown method"},
		{name: "update_config without projects", method: "update_config", params: `{"project_name":"proj","config":{"git_sync":true}}`},
		{name: "update_config bad json", method: "update_config", params: `not-json`},
		{name: "takeover bad json", method: "takeover", params: `not-json`},
		{name: "takeover pid=0", method: "takeover", params: `{"pid":0,"session_id":"sess-abc"}`},
		{name: "takeover invalid session_id", method: "takeover", params: `{"pid":` + pid + `,"session_id":"not-a-valid-uuid","proc_start_time":1}`},
		{name: "takeover proc_start_time=0", method: "takeover", params: `{"pid":12345,"session_id":"12345678-1234-1234-1234-123456789012","proc_start_time":0}`},
		{name: "close_discovered bad json", method: "close_discovered", params: `not-json`},
		{name: "close_discovered pid=0", method: "close_discovered", params: `{"pid":0}`},
		{name: "close_discovered proc_start_time=0", method: "close_discovered", params: `{"pid":12345,"proc_start_time":0}`},
		{name: "close_discovered invalid session_id", method: "close_discovered", params: `{"pid":12345,"session_id":"not-a-uuid","proc_start_time":1}`},
		{name: "set_session_label missing key", method: "set_session_label", params: `{"label":"x"}`},
		{name: "set_session_label too long", method: "set_session_label", params: `{"key":"k","label":"` + strings.Repeat("a", 129) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, Discovery{}, nil)
			req := node.ReverseMsg{Method: tc.method}
			if tc.params != "" {
				req.Params = json.RawMessage(tc.params)
			}
			_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want to contain %q", err, tc.wantErr)
			}
		})
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

	cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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

	cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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

	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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

	cfg := &Config{
		URL:         wsURL(srv),
		NodeID:      "my-node",
		Token:       "my-token",
		DisplayName: "Test Node",
	}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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

	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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

	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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

	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), mgr, Discovery{}, nil)

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

	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), mgr, Discovery{}, nil)

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

// ---- handleRequest: fetch_projects with real project manager ----

func TestHandleRequest_FetchProjects_WithMgr(t *testing.T) {
	root := t.TempDir()
	projDir := root + "/myproject"
	os.MkdirAll(projDir, 0755)
	os.WriteFile(projDir+"/CLAUDE.md", []byte("# proj"), 0644)

	mgr, _ := project.NewManager(root, project.PlannerDefaults{})
	mgr.Scan()

	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), mgr, Discovery{}, nil)

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

	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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

	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(router), nil, Discovery{}, nil)

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

	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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

	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	router := makeRouter()
	// Seed a session so the dispatch path has something to mutate.
	proc := session.NewTestProcess()
	router.InjectSession("feishu:direct:alice:general", proc)
	c := New(cfg, testRouter(router), nil, Discovery{}, nil)

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
	if got := router.SessionFor("feishu:direct:alice:general").UserLabel(); got != "标记" {
		t.Errorf("UserLabel = %q, want 标记", got)
	}
}

func TestHandleRequest_SetSessionLabel_UnknownKey(t *testing.T) {
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

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
