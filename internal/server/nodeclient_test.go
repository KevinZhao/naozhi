package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

// ─── Dashboard aggregation tests ─────────────────────────────────────────────

func TestHandleAPISessions_NoNodes(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)

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
	srv.nodes.Add("macbook", node.NewHTTPClient("macbook", remote.URL, "", "MacBook"))
	srv.nodes.SetKnown("macbook", "MacBook")
	// Pre-populate cache
	srv.nodeCache.RefreshAll()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)

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
	srv.nodes.Add("bad-node", node.NewHTTPClient("bad-node", remote.URL, "", "Bad"))
	srv.nodes.SetKnown("bad-node", "Bad")
	// Pre-populate cache
	srv.nodeCache.RefreshAll()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)

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
		json.NewEncoder(w).Encode([]clievent.EventEntry{
			{Time: 1000, Type: "text", Summary: "hello"},
		})
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes.Add("macbook", node.NewHTTPClient("macbook", remote.URL, "", "MacBook"))

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key=test:d:u:general&node=macbook", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var entries []clievent.EventEntry
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
	srv.sessionH.HandleEvents(w, req)

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

	srv, hs := newTestServerHS(&mockPlatform{})
	srv.nodes.Add("macbook", node.NewHTTPClient("macbook", remote.URL, "", "MacBook"))

	body := `{"key":"test:d:u:general","text":"hello","node":"macbook"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hs.sendH.handleSend(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	hs.sendH.engine.wg.Wait()

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
	_, hs := newTestServerHS(&mockPlatform{})

	body := `{"key":"test:d:u:general","text":"hello","node":"unknown"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hs.sendH.handleSend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleAPISend_LocalNodeExplicit(t *testing.T) {
	_, hs := newTestServerHS(&mockPlatform{})

	body := `{"key":"p:t:u:general","text":"hi","node":"local"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hs.sendH.handleSend(w, req)

	// Should use local path and return 202 (accepted)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
}

// ─── Hub remote handler tests ────────────────────────────────────────────────

func TestHub_RemoteSend_UnknownNode(t *testing.T) {
	hub, _ := newTestHub(t, "")
	client := newTestWSClient()

	hub.handleSend(client, node.ClientMsg{
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
		send: make(chan []byte, 256),
		done: make(chan struct{}),
	}
	c.authenticated.Store(true)
	return c
}

// readClientMsg returns c's next frame that is not a workflow push loop's.
func readClientMsg(t *testing.T, c *wsClient, timeout time.Duration) node.ServerMsg {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case data := <-c.send:
			var msg node.ServerMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !isWorkflowFrame(msg.Type) {
				return msg
			}
		case <-deadline:
			t.Fatal("timeout reading client message")
			return node.ServerMsg{}
		}
	}
}
