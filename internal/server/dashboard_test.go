package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// ─── handleAPISessionEvents ──────────────────────────────────────────────────

func TestHandleAPISessionEvents_MissingKey(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleEvents(w, req)

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
	srv.sessionH.handleEvents(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "session not found") {
		t.Errorf("body = %q, want 'session not found'", w.Body.String())
	}
}

// seedEventSession injects a session with the given entries and returns its key.
// Extracted so pagination tests can share setup.
func seedEventSession(t *testing.T, srv *Server, times ...int64) string {
	t.Helper()
	key := "test:d:u:general"
	proc := session.NewTestProcess()
	for _, ts := range times {
		proc.EventLog.Append(cli.EventEntry{Time: ts, Type: "text", Summary: "msg"})
	}
	srv.router.InjectSession(key, proc)
	return key
}

func TestHandleAPISessionEvents_LimitCapsInitialFetch(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000, 2000, 3000, 4000, 5000)

	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key="+key+"&limit=2", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entries []cli.EventEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	// Newest two in chronological order = times 4000, 5000.
	if entries[0].Time != 4000 || entries[1].Time != 5000 {
		t.Errorf("entries times = [%d, %d], want [4000, 5000]",
			entries[0].Time, entries[1].Time)
	}
}

func TestHandleAPISessionEvents_BeforePaginatesBackwards(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000, 2000, 3000, 4000, 5000)

	// Page 1: before=3500, limit=10 → Time < 3500 → {1000,2000,3000}.
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key="+key+"&before=3500&limit=10", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entries []cli.EventEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if entries[0].Time != 1000 || entries[2].Time != 3000 {
		t.Errorf("entries times = [%d..%d], want [1000..3000]",
			entries[0].Time, entries[2].Time)
	}
}

func TestHandleAPISessionEvents_BeforeAndLimitPrefersNewest(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000, 2000, 3000, 4000, 5000)

	// before=5000 + limit=2 → the two newest entries strictly older than 5000
	// → {3000, 4000}.
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key="+key+"&before=5000&limit=2", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleEvents(w, req)

	var entries []cli.EventEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Time != 3000 || entries[1].Time != 4000 {
		t.Errorf("entries times = [%d, %d], want [3000, 4000]",
			entries[0].Time, entries[1].Time)
	}
}

func TestHandleAPISessionEvents_InvalidBefore(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	seedEventSession(t, srv, 1000)

	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key=test:d:u:general&before=not-a-number", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleEvents(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleAPISessionEvents_InvalidLimit(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	seedEventSession(t, srv, 1000)

	for _, v := range []string{"abc", "-1"} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/sessions/events?key=test:d:u:general&limit="+v, nil)
		w := httptest.NewRecorder()
		srv.sessionH.handleEvents(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("limit=%q: status = %d, want 400", v, w.Code)
		}
	}
}

func TestHandleAPISessionEvents_LimitClampedAtMax(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	// Seed only 3 events; ensure asking for 10000 doesn't blow up and we
	// still get all available entries back (limit clamp is server-side).
	seedEventSession(t, srv, 1000, 2000, 3000)

	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key=test:d:u:general&limit=10000", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entries []cli.EventEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("len = %d, want 3", len(entries))
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
	srv.sendH.handleSend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "key is required") {
		t.Errorf("body = %q, want 'key is required'", w.Body.String())
	}
}

// TestHandleAPISend_TextTooLong_JSON asserts the JSON handleSend branch
// enforces the same per-field text cap as the WS path. Pre-R60, an
// oversized text payload could pass the body-level MaxBytesReader and
// drive a multi-MB CLI stdin write once CoalesceMessages ran. R60-SEC-2.
func TestHandleAPISend_TextTooLong_JSON(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	big := strings.Repeat("x", maxWSSendTextBytes+1)
	body := `{"key":"p:t:u:general","text":"` + big + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.sendH.handleSend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "too long") {
		t.Errorf("body = %q, want 'too long'", w.Body.String())
	}
}

// TestHandleAPISend_RejectControlInKey asserts the HTTP handler refuses keys
// with C0 control bytes at the boundary — sessionSend also rejects them, but
// the R60-SEC-8 gate runs BEFORE any slog attr is written, so an attacker
// cannot fragment log lines through the "workspace validation failed" path
// using an ANSI-padded key. R60-SEC-8.
func TestHandleAPISend_RejectControlInKey(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	body := `{"key":"p:t:u\nadmin:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.sendH.handleSend(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid key character") {
		t.Errorf("body = %q, want 'invalid key character'", w.Body.String())
	}
}

func TestHandleAPISend_MissingTextAndFiles(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"key":"p:t:u:general"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.sendH.handleSend(w, req)

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
	srv.sendH.handleSend(w, req)

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
	srv.mux.ServeHTTP(w, req)

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
	srv.mux.ServeHTTP(w, req)

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
	srv.mux.ServeHTTP(w, req)

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
	srv.sendH.handleSend(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
}

func TestHandleAPISend_InterruptWhenBusy(t *testing.T) {
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
	srv.sendH.handleSend(w, req)

	// With interrupt-on-busy, the API accepts immediately and interrupts
	// the running session; the goroutine will timeout waiting for the guard.
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("status = %q, want 'accepted'", resp["status"])
	}
}

func TestHandleAPISend_ResponseIsJSON(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := bytes.NewBufferString(`{"key":"x:y:z:general","text":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.sendH.handleSend(w, req)

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
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)

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
	srv.registerDashboard()

	key := "dashboard:direct:test-session:general"
	body := `{"key":"` + key + `","text":"hi","workspace":"` + tmpDir + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.sendH.handleSend(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	// Verify workspace was set for the chat key. validateWorkspace calls
	// filepath.EvalSymlinks, which on macOS rewrites /var/folders/... to
	// /private/var/folders/... and /tmp/... to /private/tmp/... — resolve
	// the expected value the same way so the assertion is platform-neutral.
	chatKey := "dashboard:direct:test-session"
	ws := router.GetWorkspace(chatKey)
	wantDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", tmpDir, err)
	}
	if ws != wantDir {
		t.Errorf("workspace = %q, want %q", ws, wantDir)
	}
}

func TestHandleAPISend_WorkspaceInvalidDir(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{
		Workspace: "/default/workspace",
	})
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{})
	srv.registerDashboard()

	key := "dashboard:direct:test-session:general"
	body := `{"key":"` + key + `","text":"hi","workspace":"/nonexistent/path/xyz"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.sendH.handleSend(w, req)

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
	srv.nodes["macbook"] = node.NewHTTPClient("macbook", remote.URL, "", "MacBook Pro")
	srv.knownNodes["macbook"] = "MacBook Pro"

	// Populate the cache synchronously
	srv.nodeCache.RefreshAll()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)

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

// ─── session.ValidateUserLabel ──────────────────────────────────────────────
// Validation logic lives in the session package (session.ValidateUserLabel)
// so the dashboard HTTP path and the reverse-RPC worker share one rule set.
// Tab is rejected (slog.TextHandler uses it as key/value separator) and C1
// control range is rejected. R64-GO-H2 / L2.

func TestValidateUserLabel(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty passes", "", "", false},
		{"whitespace-only passes (treated as clear)", "   ", "", false},
		{"ASCII label", "hello", "hello", false},
		{"Chinese label", "重构会话", "重构会话", false},
		{"trims surrounding space", "  hi  ", "hi", false},
		{"128 bytes exact", strings.Repeat("a", 128), strings.Repeat("a", 128), false},
		{"129 bytes rejected", strings.Repeat("a", 129), "", true},
		{"tab rejected", "a\tb", "", true},
		{"newline rejected", "a\nb", "", true},
		{"NUL rejected", "a\x00b", "", true},
		{"DEL rejected", "a\x7fb", "", true},
		{"C1 control rejected (U+0085)", "a\u0085b", "", true},
		{"invalid utf-8 rejected", "\xc3\x28", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := session.ValidateUserLabel(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("got = %q, want = %q", got, c.want)
			}
		})
	}
}

// ─── handleSetLabel (PATCH /api/sessions/label) ──────────────────────────────

// TestHandleSetLabel_OK verifies the happy path: an existing session accepts
// a label update and the mutation is visible via Router.GetSession.
func TestHandleSetLabel_OK(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000)

	body := `{"key":"` + key + `","label":"重构会话"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleSetLabel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if got := srv.router.GetSession(key).UserLabel(); got != "重构会话" {
		t.Errorf("router UserLabel = %q, want 重构会话", got)
	}
}

// TestHandleSetLabel_EmptyClears verifies that an empty label clears any
// prior label (the documented "reset to auto-title" gesture).
func TestHandleSetLabel_EmptyClears(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000)
	srv.router.SetUserLabel(key, "before")

	body := `{"key":"` + key + `","label":""}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleSetLabel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := srv.router.GetSession(key).UserLabel(); got != "" {
		t.Errorf("UserLabel = %q, want empty after clear", got)
	}
}

// TestHandleSetLabel_TooLong rejects labels above session.MaxUserLabelBytes.
func TestHandleSetLabel_TooLong(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000)

	label := strings.Repeat("a", session.MaxUserLabelBytes+1)
	body := `{"key":"` + key + `","label":"` + label + `"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleSetLabel(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetLabel_ControlChar rejects labels carrying ASCII control bytes
// that would poison slog JSONHandler output and dashboard HTML.
func TestHandleSetLabel_ControlChar(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000)

	// \n in JSON string form
	body := `{"key":"` + key + `","label":"line1\nline2"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleSetLabel(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetLabel_NotFound returns 404 for an unknown key.
func TestHandleSetLabel_NotFound(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"key":"no:such:key","label":"x"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleSetLabel(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestHandleSetLabel_MissingKey rejects requests without a key.
func TestHandleSetLabel_MissingKey(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"label":"x"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleSetLabel(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleSetLabel_RemoteProxy verifies that a node-scoped request is
// forwarded to the remote's PATCH /api/sessions/label endpoint.
func TestHandleSetLabel_RemoteProxy(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   string
	)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "label": "remote-label"})
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes["macbook"] = node.NewHTTPClient("macbook", remote.URL, "", "MacBook Pro")
	srv.knownNodes["macbook"] = "MacBook Pro"

	body := `{"key":"feishu:direct:alice:general","node":"macbook","label":"remote-label"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleSetLabel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("remote method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/api/sessions/label" {
		t.Errorf("remote path = %q, want /api/sessions/label", gotPath)
	}
	if !strings.Contains(gotBody, "remote-label") {
		t.Errorf("remote body = %q, want it to contain 'remote-label'", gotBody)
	}
}

// ─── handleResume last_prompt charset (R65-SEC-M-3) ──────────────────────────

// TestHandleResume_LastPromptC1Rejected verifies that a C1 control codepoint
// inside last_prompt (arriving as valid UTF-8 continuation bytes 0xC2 0x85 for
// U+0085 NEL) is rejected. The prior byte-only loop only checked `c < 0x20`,
// which misses C1: continuation byte 0x85 falls in 0x80..0xBF and survived
// the check. R65-SEC-M-3 regression.
func TestHandleResume_LastPromptC1Rejected(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	// Valid UUID + C1 NEL (U+0085) inside last_prompt.
	body := `{"session_id":"12345678-1234-1234-1234-123456789abc","last_prompt":"hi\u0085there"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/resume", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleResume(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid control characters") {
		t.Errorf("body = %q, want 'invalid control characters' message", w.Body.String())
	}
}

// TestHandleResume_LastPromptTabAllowed confirms tab remains acceptable inside
// last_prompt — slog escapes \t in JSONHandler, and operators sometimes paste
// tab-delimited snippets. R65-SEC-M-3.
func TestHandleResume_LastPromptTabAllowed(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	body := `{"session_id":"12345678-1234-1234-1234-123456789abc","last_prompt":"col1\tcol2"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/resume", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.handleResume(w, req)

	// Accept any non-400 here (the resume path may go on to 200 or return a
	// server-specific error depending on router state). The point is that
	// the charset gate did not reject tab.
	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "invalid control characters") {
		t.Errorf("tab should be allowed, got 400 with charset error: %q", w.Body.String())
	}
}

// TestUptimeString_CachesWithinSecondBucket locks down R65-PERF-L-1: two
// calls within the same second return the same string and share the same
// underlying snapshot pointer (no re-format).
func TestUptimeString_CachesWithinSecondBucket(t *testing.T) {
	// Start 5 seconds ago so rounding lands on a stable integer bucket.
	h := &SessionHandlers{startedAt: time.Now().Add(-5 * time.Second)}

	first := h.uptimeString()
	snap1 := h.uptimeCache.Load()
	if snap1 == nil {
		t.Fatal("uptimeCache not populated after first call")
	}
	second := h.uptimeString()
	snap2 := h.uptimeCache.Load()

	if first != second {
		t.Errorf("uptimeString within same bucket returned different values: %q vs %q", first, second)
	}
	if snap1 != snap2 {
		t.Errorf("expected cached snapshot pointer to be reused within the same bucket")
	}
	if first == "" {
		t.Error("expected non-empty uptime string")
	}
}

// TestUptimeString_RotatesAcrossBuckets confirms the cache invalidates once
// the integer-second bucket advances (startedAt pushed backwards simulates
// the passage of time).
func TestUptimeString_RotatesAcrossBuckets(t *testing.T) {
	h := &SessionHandlers{startedAt: time.Now().Add(-1 * time.Second)}
	first := h.uptimeString()
	// Shift startedAt back so the bucket id increases by at least one second.
	h.startedAt = h.startedAt.Add(-2 * time.Second)
	second := h.uptimeString()
	if first == second {
		t.Errorf("expected uptime to advance after bucket rotation, both = %q", first)
	}
}

// ─── R67 regressions ─────────────────────────────────────────────────────────

// TestHandlePreview_RejectsInvalidNodeID locks down R67-SEC-2: the preview
// handler must run `nodeID` through LookupNode (which enforces the
// [a-zA-Z0-9._-] allowlist) before logging or proxying. Before the fix,
// `GetNode(nodeID)` passed raw nodeID strings — including newlines and
// ANSI escapes — into the subsequent slog.Warn attr, breaking log parsing.
func TestHandlePreview_RejectsInvalidNodeID(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	// Valid session_id shape (length + charset match IsValidSessionID) but
	// malicious nodeID containing a newline — MUST be rejected at the
	// LookupNode boundary with 400, not fall through to the slog.Warn path.
	u := "/api/discovered/preview?session_id=12345678-1234-1234-1234-123456789abc&node=bad%0Anode"
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.discoveryH.handlePreview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%q)", w.Code, w.Body.String())
	}
}

// TestHandleTakeover_RejectsTraversalCWD locks down R67-SEC-7: `..`
// segments in the takeover CWD must be rejected before filepath.Clean
// collapses them. Previously, `/home/../etc` silently Clean'd to `/etc`
// and, when `allowedRoot == ""`, the CLI was spawned in the wrong
// directory.
func TestHandleTakeover_RejectsTraversalCWD(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	// Inject a non-empty claudeDir so the R67-SEC-4 "discovery not
	// available" early return does not fire; we're targeting the CWD
	// gate specifically. Using t.TempDir() to avoid touching real state.
	srv.discoveryH.claudeDir = t.TempDir()

	body := `{"pid":99999,"session_id":"12345678-1234-1234-1234-123456789abc","cwd":"/home/../etc","proc_start_time":1234}`
	req := httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.discoveryH.handleTakeover(w, req)

	// Without the fix, this traversal would either pass (allowedRoot empty)
	// or be rejected much later. The fix returns 400 at the validation gate
	// before any kill attempt.
	if w.Code == http.StatusOK || w.Code == http.StatusAccepted {
		t.Errorf("traversal CWD accepted (status=%d) — validateRemoteWorkspace did not fire", w.Code)
	}
}

// TestHandleTakeover_NoClaudeDirRefuses locks down R67-SEC-4: when
// claudeDir is unavailable there is no discovered list to cross-check
// against, so any pid+start_time accepts arbitrary SIGTERM targets. The
// handler must return 503 like handleClose does.
func TestHandleTakeover_NoClaudeDirRefuses(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.discoveryH.claudeDir = "" // simulate "discovery not available"

	body := `{"pid":1,"session_id":"12345678-1234-1234-1234-123456789abc","cwd":"/tmp","proc_start_time":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.discoveryH.handleTakeover(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (body=%q)", w.Code, w.Body.String())
	}
}

// TestHistorySessions_EmptyHistoryCached locks down R67-GO-5: when the
// underlying FS has no history entries, loadHistorySessions stores a
// legitimate (nil, now()) cache tuple; the singleflight double-check must
// treat this as "cached" and NOT re-run the scan on subsequent calls. The
// prior `historyCache != nil` guard misclassified empty-history results
// as "not cached" and drove a redundant FS scan per TTL window.
func TestHistorySessions_EmptyHistoryCached(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	// Empty claudeDir so loadHistorySessions naturally yields nil.
	srv.sessionH.claudeDir = t.TempDir()

	// First call: miss → loadHistorySessions → stores (nil, now()).
	_ = srv.sessionH.historySessions()

	// Capture the cache time — the whole point of the fix is that it is
	// NON-ZERO after a successful load of empty history.
	srv.sessionH.historyCacheMu.Lock()
	cacheTimeAfterFirst := srv.sessionH.historyCacheTime
	srv.sessionH.historyCacheMu.Unlock()
	if cacheTimeAfterFirst.IsZero() {
		t.Fatal("historyCacheTime is zero after load — loadHistorySessions did not store the sentinel")
	}

	// Second immediate call: hit → must NOT update historyCacheTime
	// because it is still within the 120s TTL window.
	_ = srv.sessionH.historySessions()

	srv.sessionH.historyCacheMu.Lock()
	cacheTimeAfterSecond := srv.sessionH.historyCacheTime
	srv.sessionH.historyCacheMu.Unlock()
	if !cacheTimeAfterSecond.Equal(cacheTimeAfterFirst) {
		t.Errorf("empty-history cache was re-loaded — cacheTime changed from %v to %v", cacheTimeAfterFirst, cacheTimeAfterSecond)
	}
}
