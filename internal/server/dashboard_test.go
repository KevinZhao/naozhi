package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// ─── handleAPISessionEvents ──────────────────────────────────────────────────

func TestHandleAPISessionEvents_MissingKey(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessionEvents(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing key") {
		t.Errorf("body = %q, want 'missing key'", w.Body.String())
	}
}

func TestHandleAPISessionEvents_SessionNotFound(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key=no-such-key", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessionEvents(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "session not found") {
		t.Errorf("body = %q, want 'session not found'", w.Body.String())
	}
}

// ─── handleAPISend ────────────────────────────────────────────────────────────

func TestHandleAPISend_MissingKeyJSON(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"text":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "key is required") {
		t.Errorf("body = %q, want 'key is required'", w.Body.String())
	}
}

func TestHandleAPISend_MissingTextAndFiles(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"key":"p:t:u:general"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "text or files") {
		t.Errorf("body = %q, want 'text or files'", w.Body.String())
	}
}

func TestHandleAPISend_InvalidJSON(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleAPISend_UnauthorizedNoToken(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleAPISend_UnauthorizedWrongToken(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleAPISend_AcceptedWithValidToken(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("status = %q, want accepted", resp["status"])
	}
	if resp["key"] != "p:t:u:general" {
		t.Errorf("key = %q, want p:t:u:general", resp["key"])
	}
}

func TestHandleAPISend_AcceptedNoAuth(t *testing.T) {
	srv := newTestServer(&mockPlatform{}) // no dashboardToken

	body := `{"key":"p:t:u:general","text":"hello world"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
}

func TestHandleAPISend_ConflictWhenBusy(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := "p:t:u:general"

	// Manually acquire the session guard to simulate a busy session.
	srv.sessionGuard.TryAcquire(key)
	defer srv.sessionGuard.Release(key)

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "session busy" {
		t.Errorf("error = %q, want 'session busy'", resp["error"])
	}
}

func TestHandleAPISend_ResponseIsJSON(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := bytes.NewBufferString(`{"key":"x:y:z:general","text":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// ─── handleAPISessions: stats include agents and workspace ──────────────────

func TestHandleAPISessions_StatsIncludeAgentsAndWorkspace(t *testing.T) {
	agents := map[string]session.AgentOpts{
		"code-reviewer": {Model: "sonnet"},
		"researcher":    {Model: "opus"},
	}
	router := session.NewRouter(session.RouterConfig{
		MaxProcs:  5,
		Workspace: "/test/workspace",
	})
	srv := New(":0", router, nil, agents, nil, nil, "claude", ServerOptions{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	stats, ok := resp["stats"].(map[string]any)
	if !ok {
		t.Fatal("stats field missing")
	}

	// Check max_procs
	if mp, ok := stats["max_procs"].(float64); !ok || int(mp) != 5 {
		t.Errorf("max_procs = %v, want 5", stats["max_procs"])
	}

	// Check default_workspace
	if ws, ok := stats["default_workspace"].(string); !ok || ws != "/test/workspace" {
		t.Errorf("default_workspace = %v, want /test/workspace", stats["default_workspace"])
	}

	// Check agents list includes "general" and configured agents
	agentsList, ok := stats["agents"].([]any)
	if !ok {
		t.Fatal("agents field missing or not array")
	}
	agentSet := make(map[string]bool)
	for _, a := range agentsList {
		agentSet[a.(string)] = true
	}
	if !agentSet["general"] {
		t.Error("agents should include 'general'")
	}
	if !agentSet["code-reviewer"] {
		t.Error("agents should include 'code-reviewer'")
	}
	if !agentSet["researcher"] {
		t.Error("agents should include 'researcher'")
	}
}

// ─── handleAPISend: workspace override ──────────────────────────────────────

func TestHandleAPISend_WorkspaceOverride(t *testing.T) {
	tmpDir := t.TempDir()
	router := session.NewRouter(session.RouterConfig{
		Workspace: "/default/workspace",
	})
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{})

	key := "dashboard:direct:test-session:general"
	body := `{"key":"` + key + `","text":"hi","workspace":"` + tmpDir + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	// Verify workspace was set for the chat key
	chatKey := "dashboard:direct:test-session"
	ws := router.GetWorkspace(chatKey)
	if ws != tmpDir {
		t.Errorf("workspace = %q, want %q", ws, tmpDir)
	}
}

func TestHandleAPISend_WorkspaceInvalidDir(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{
		Workspace: "/default/workspace",
	})
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{})

	key := "dashboard:direct:test-session:general"
	body := `{"key":"` + key + `","text":"hi","workspace":"/nonexistent/path/xyz"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAPISend(w, req)

	// Invalid workspace should be rejected with 403
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	// Verify workspace was NOT set
	chatKey := "dashboard:direct:test-session"
	ws := router.GetWorkspace(chatKey)
	if ws != "/default/workspace" {
		t.Errorf("workspace = %q, want /default/workspace (invalid path should be rejected)", ws)
	}
}

// ─── multi-node API routing ──────────────────────────────────────────────────

// TestHandleAPISessions_NodeAggregation verifies that /api/sessions merges remote
// node sessions (from cache) and includes the nodes status map.
func TestHandleAPISessions_NodeAggregation(t *testing.T) {
	// Mock remote node serving /api/sessions with one session
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sessions":
			json.NewEncoder(w).Encode(map[string]any{
				"sessions": []map[string]any{
					{"key": "feishu:direct:bob:general", "state": "ready"},
				},
			})
		case "/api/projects":
			json.NewEncoder(w).Encode([]map[string]any{})
		case "/api/discovered":
			json.NewEncoder(w).Encode([]map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes["macbook"] = NewNodeClient("macbook", remote.URL, "", "MacBook Pro")
	srv.knownNodes["macbook"] = "MacBook Pro"

	// Populate the cache synchronously
	srv.nodeCache.RefreshAll()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.handleAPISessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// nodes map must include macbook
	nodes, ok := resp["nodes"].(map[string]any)
	if !ok {
		t.Fatal("nodes field missing")
	}
	if _, ok := nodes["macbook"]; !ok {
		t.Error("nodes should contain 'macbook'")
	}
	if macbook, ok := nodes["macbook"].(map[string]any); ok {
		if macbook["status"] != "ok" {
			t.Errorf("macbook status = %v, want ok", macbook["status"])
		}
		if macbook["display_name"] != "MacBook Pro" {
			t.Errorf("macbook display_name = %v, want 'MacBook Pro'", macbook["display_name"])
		}
	}

	// sessions must include the remote session with node="macbook"
	sessions, ok := resp["sessions"].([]any)
	if !ok {
		t.Fatal("sessions field missing")
	}
	var found bool
	for _, s := range sessions {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if sm["key"] == "feishu:direct:bob:general" && sm["node"] == "macbook" {
			found = true
			break
		}
	}
	if !found {
		t.Error("remote session with node='macbook' not found in aggregated sessions")
	}
}
