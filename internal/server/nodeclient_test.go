package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// ─── NodeClient tests ────────────────────────────────────────────────────────

func TestNodeClient_FetchSessions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions" {
			http.Error(w, "not found", 404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{
				{"key": "feishu:direct:alice:general", "state": "ready"},
				{"key": "feishu:direct:bob:general", "state": "running"},
			},
		})
	}))
	defer ts.Close()

	nc := NewNodeClient("test", ts.URL, "test-token", "Test Node")
	sessions, err := nc.FetchSessions(context.Background())
	if err != nil {
		t.Fatalf("FetchSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0]["key"] != "feishu:direct:alice:general" {
		t.Errorf("session[0].key = %v", sessions[0]["key"])
	}
}

func TestNodeClient_FetchSessions_NoAuth(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{}})
	}))
	defer ts.Close()

	nc := NewNodeClient("test", ts.URL, "", "Test")
	_, err := nc.FetchSessions(context.Background())
	if err != nil {
		t.Fatalf("FetchSessions: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("expected no auth header, got %q", gotAuth)
	}
}

func TestNodeClient_FetchSessions_AuthFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", 401)
	}))
	defer ts.Close()

	nc := NewNodeClient("test", ts.URL, "wrong-token", "Test")
	_, err := nc.FetchSessions(context.Background())
	if err == nil {
		t.Fatal("expected error for auth failure")
	}
}

func TestNodeClient_FetchSessions_MalformedResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer ts.Close()

	nc := NewNodeClient("test", ts.URL, "", "Test")
	_, err := nc.FetchSessions(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed response")
	}
}

func TestNodeClient_FetchEvents(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions/events" {
			http.Error(w, "not found", 404)
			return
		}
		key := r.URL.Query().Get("key")
		if key != "test:d:u:general" {
			http.Error(w, "bad key", 400)
			return
		}
		after := r.URL.Query().Get("after")
		if after != "1500" {
			http.Error(w, "bad after", 400)
			return
		}
		json.NewEncoder(w).Encode([]cli.EventEntry{
			{Time: 2000, Type: "text", Summary: "hello"},
			{Time: 3000, Type: "result", Summary: "done"},
		})
	}))
	defer ts.Close()

	nc := NewNodeClient("test", ts.URL, "", "Test")
	entries, err := nc.FetchEvents(context.Background(), "test:d:u:general", 1500)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Type != "text" || entries[1].Type != "result" {
		t.Errorf("unexpected entries: %+v", entries)
	}
}

func TestNodeClient_FetchEvents_NoAfter(t *testing.T) {
	var gotAfter string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAfter = r.URL.Query().Get("after")
		json.NewEncoder(w).Encode([]cli.EventEntry{})
	}))
	defer ts.Close()

	nc := NewNodeClient("test", ts.URL, "", "Test")
	_, err := nc.FetchEvents(context.Background(), "key", 0)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if gotAfter != "" {
		t.Errorf("expected no after param, got %q", gotAfter)
	}
}

func TestNodeClient_Send(t *testing.T) {
	var gotKey, gotText, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions/send" {
			http.Error(w, "not found", 404)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Key  string `json:"key"`
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotKey = body.Key
		gotText = body.Text
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}))
	defer ts.Close()

	nc := NewNodeClient("test", ts.URL, "my-token", "Test")
	err := nc.Send(context.Background(), "test:d:u:general", "hello world", "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotKey != "test:d:u:general" {
		t.Errorf("key = %q", gotKey)
	}
	if gotText != "hello world" {
		t.Errorf("text = %q", gotText)
	}
	if gotAuth != "Bearer my-token" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestNodeClient_Send_Unreachable(t *testing.T) {
	nc := NewNodeClient("test", "http://127.0.0.1:1", "", "Test")
	nc.httpClient.Timeout = 1 * time.Second
	err := nc.Send(context.Background(), "key", "text", "")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

// ─── Dashboard aggregation tests ─────────────────────────────────────────────

func TestHandleAPISessions_NoNodes(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	// No nodes field when no remote nodes configured
	if _, ok := resp["nodes"]; ok {
		t.Error("expected no 'nodes' field without remote nodes configured")
	}
}

func TestHandleAPISessions_WithRemoteNodes(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{
				{"key": "feishu:direct:bob:general", "state": "ready"},
			},
		})
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes["macbook"] = NewNodeClient("macbook", remote.URL, "", "MacBook")
	srv.knownNodes["macbook"] = "MacBook"
	// Pre-populate cache
	srv.nodeCache.RefreshAll()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	sessions, ok := resp["sessions"].([]any)
	if !ok {
		t.Fatal("sessions not an array")
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session (0 local + 1 remote), got %d", len(sessions))
	}

	nodes, ok := resp["nodes"].(map[string]any)
	if !ok {
		t.Fatal("expected nodes map")
	}
	if _, ok := nodes["local"]; !ok {
		t.Error("expected 'local' in nodes")
	}
	if _, ok := nodes["macbook"]; !ok {
		t.Error("expected 'macbook' in nodes")
	}
	macbook := nodes["macbook"].(map[string]any)
	if macbook["status"] != "ok" {
		t.Errorf("macbook status = %v, want ok", macbook["status"])
	}
}

func TestHandleAPISessions_RemoteNodeError(t *testing.T) {
	// Remote returns error
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes["bad-node"] = NewNodeClient("bad-node", remote.URL, "", "Bad")
	srv.knownNodes["bad-node"] = "Bad"
	// Pre-populate cache
	srv.nodeCache.RefreshAll()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (should not fail entire request)", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	nodes := resp["nodes"].(map[string]any)
	badNode := nodes["bad-node"].(map[string]any)
	if badNode["status"] != "error" {
		t.Errorf("bad-node status = %v, want error", badNode["status"])
	}
}

func TestHandleAPISessionEvents_RemoteNode(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "test:d:u:general" {
			http.Error(w, "bad key", 400)
			return
		}
		json.NewEncoder(w).Encode([]cli.EventEntry{
			{Time: 1000, Type: "text", Summary: "hello"},
		})
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes = map[string]NodeConn{
		"macbook": NewNodeClient("macbook", remote.URL, "", "MacBook"),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key=test:d:u:general&node=macbook", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessionEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var entries []cli.EventEntry
	json.NewDecoder(w.Body).Decode(&entries)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Summary != "hello" {
		t.Errorf("entry summary = %q", entries[0].Summary)
	}
}

func TestHandleAPISessionEvents_UnknownNode(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key=test:d:u:general&node=unknown", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessionEvents(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleAPISend_RemoteNode(t *testing.T) {
	var mu sync.Mutex
	var gotKey, gotText string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key  string `json:"key"`
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		gotKey = body.Key
		gotText = body.Text
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes = map[string]NodeConn{
		"macbook": NewNodeClient("macbook", remote.URL, "", "MacBook"),
	}

	body := `{"key":"test:d:u:general","text":"hello","node":"macbook"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	// Wait for the async goroutine
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	k, tx := gotKey, gotText
	mu.Unlock()
	if k != "test:d:u:general" {
		t.Errorf("key = %q", k)
	}
	if tx != "hello" {
		t.Errorf("text = %q", tx)
	}
}

func TestHandleAPISend_UnknownNode(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.nodes = map[string]NodeConn{}

	body := `{"key":"test:d:u:general","text":"hello","node":"unknown"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleAPISend_LocalNodeExplicit(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"key":"p:t:u:general","text":"hi","node":"local"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	// Should use local path and return 202 (accepted)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
}

// ─── Hub remote handler tests ────────────────────────────────────────────────

func TestHub_RemoteSend(t *testing.T) {
	var mu sync.Mutex
	var gotKey, gotText string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sessions/send" {
			var body struct {
				Key  string `json:"key"`
				Text string `json:"text"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			gotKey = body.Key
			gotText = body.Text
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
		}
	}))
	defer ts.Close()

	nodes := map[string]NodeConn{
		"remote": NewNodeClient("remote", ts.URL, "", "Remote"),
	}
	router := session.NewRouter(session.RouterConfig{})
	guard := newSessionGuard()
	var nodesMu sync.RWMutex
	hub := NewHub(router, nil, nil, "", guard, nodes, &nodesMu, nil)
	defer hub.Shutdown()

	client := newTestWSClient()
	hub.handleSend(client, wsClientMsg{
		Type: "send",
		Key:  "test:d:u:general",
		Text: "hello",
		Node: "remote",
		ID:   "r1",
	})

	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "send_ack" {
		t.Errorf("type = %q, want send_ack", msg.Type)
	}
	if msg.Status != "accepted" {
		t.Errorf("status = %q, want accepted", msg.Status)
	}

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	k, tx := gotKey, gotText
	mu.Unlock()
	if k != "test:d:u:general" {
		t.Errorf("key = %q", k)
	}
	if tx != "hello" {
		t.Errorf("text = %q", tx)
	}
}

func TestHub_RemoteSend_UnknownNode(t *testing.T) {
	hub, _ := newTestHub("")
	client := newTestWSClient()

	hub.handleSend(client, wsClientMsg{
		Type: "send",
		Key:  "test:d:u:general",
		Text: "hello",
		Node: "nonexistent",
		ID:   "r2",
	})

	msg := readClientMsg(t, client, 2*time.Second)
	if msg.Type != "send_ack" {
		t.Errorf("type = %q, want send_ack", msg.Type)
	}
	if msg.Status != "error" {
		t.Errorf("status = %q, want error", msg.Status)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func newTestWSClient() *wsClient {
	c := &wsClient{
		send:          make(chan []byte, 256),
		done:          make(chan struct{}),
		subscriptions: make(map[string]func()),
	}
	c.authenticated.Store(true)
	return c
}

func readClientMsg(t *testing.T, c *wsClient, timeout time.Duration) wsServerMsg {
	t.Helper()
	select {
	case data := <-c.send:
		var msg wsServerMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return msg
	case <-time.After(timeout):
		t.Fatal("timeout reading client message")
		return wsServerMsg{}
	}
}
