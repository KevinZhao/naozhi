package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestHTTPClient returns an HTTPClient wired to srv.URL with the given token.
func newTestHTTPClient(t *testing.T, srv *httptest.Server, token string) *HTTPClient {
	t.Helper()
	c := NewHTTPClient("test-node", srv.URL, token, "Test Node")
	// Speed up tests: use a shorter timeout.
	c.httpClient.Timeout = 5 * time.Second
	return c
}

// ---- FetchSessions ----

func TestHTTPClient_FetchSessions_ok(t *testing.T) {
	want := []map[string]any{
		{"session_id": "abc", "state": "idle"},
		{"session_id": "def", "state": "busy"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"sessions": want})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "tok")
	got, err := c.FetchSessions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(got))
	}
}

func TestHTTPClient_FetchSessions_authHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer my-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"sessions": []any{}})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "my-token")
	if _, err := c.FetchSessions(context.Background()); err != nil {
		t.Fatalf("expected success with correct token, got: %v", err)
	}
}

func TestHTTPClient_FetchSessions_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	_, err := c.FetchSessions(context.Background())
	if err == nil {
		t.Fatal("expected error on 500 status")
	}
}

func TestHTTPClient_FetchSessions_badJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	_, err := c.FetchSessions(context.Background())
	if err == nil {
		t.Fatal("expected error on bad JSON")
	}
}

// ---- FetchEvents ----

func TestHTTPClient_FetchEvents_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions/events" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("key") != "feishu:group:123" {
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"time": 1000, "type": "text", "summary": "hello"},
		})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	entries, err := c.FetchEvents(context.Background(), "feishu:group:123", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

func TestHTTPClient_FetchEvents_withAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		if after != "9999" {
			http.Error(w, "bad after param", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	_, err := c.FetchEvents(context.Background(), "k", 9999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClient_FetchEvents_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	_, err := c.FetchEvents(context.Background(), "k", 0)
	if err == nil {
		t.Fatal("expected error on non-200")
	}
}

// ---- Send ----

func TestHTTPClient_Send_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/send" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if body["key"] != "feishu:group:123" || body["text"] != "hi" {
			http.Error(w, "wrong payload", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if err := c.Send(context.Background(), "feishu:group:123", "hi", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClient_Send_withWorkspace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["workspace"] != "/home/user/project" {
			http.Error(w, "missing workspace", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if err := c.Send(context.Background(), "k", "text", "/home/user/project"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClient_Send_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if err := c.Send(context.Background(), "k", "t", ""); err == nil {
		t.Fatal("expected error on non-2xx")
	}
}

// ---- FetchProjects ----

func TestHTTPClient_FetchProjects_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"name": "proj1"}})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	projs, err := c.FetchProjects(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projs) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projs))
	}
}

func TestHTTPClient_FetchProjects_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	_, err := c.FetchProjects(context.Background())
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

// ---- FetchDiscovered ----

func TestHTTPClient_FetchDiscovered_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/discovered" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"pid": 12345}})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	disc, err := c.FetchDiscovered(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(disc) != 1 {
		t.Fatalf("expected 1, got %d", len(disc))
	}
}

func TestHTTPClient_FetchDiscovered_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	_, err := c.FetchDiscovered(context.Background())
	if err == nil {
		t.Fatal("expected error on 503")
	}
}

// ---- FetchDiscoveredPreview ----

func TestHTTPClient_FetchDiscoveredPreview_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/discovered/preview" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("session_id") != "sess-abc" {
			http.Error(w, "missing session_id", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"time": 1, "type": "text"}})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	entries, err := c.FetchDiscoveredPreview(context.Background(), "sess-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

// ---- ProxyTakeover ----

func TestHTTPClient_ProxyTakeover_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/discovered/takeover" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"key": "feishu:group:123"})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	key, err := c.ProxyTakeover(context.Background(), 42, "sess-1", "/cwd", 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "feishu:group:123" {
		t.Fatalf("expected key 'feishu:group:123', got %q", key)
	}
}

func TestHTTPClient_ProxyTakeover_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	_, err := c.ProxyTakeover(context.Background(), 1, "s", "/", 0)
	if err == nil {
		t.Fatal("expected error on 409")
	}
}

// ---- ProxyCloseDiscovered ----

func TestHTTPClient_ProxyCloseDiscovered_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/discovered/close" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if err := c.ProxyCloseDiscovered(context.Background(), 42, "sess-1", "/", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClient_ProxyCloseDiscovered_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if err := c.ProxyCloseDiscovered(context.Background(), 1, "s", "/", 0); err == nil {
		t.Fatal("expected error on 404")
	}
}

// ---- ProxyRestartPlanner ----

func TestHTTPClient_ProxyRestartPlanner_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/planner/restart" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("name") != "my-project" {
			http.Error(w, "wrong name", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if err := c.ProxyRestartPlanner(context.Background(), "my-project"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClient_ProxyRestartPlanner_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if err := c.ProxyRestartPlanner(context.Background(), "proj"); err == nil {
		t.Fatal("expected error on 500")
	}
}

// ---- ProxyUpdateConfig ----

func TestHTTPClient_ProxyUpdateConfig_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/projects/config" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	cfg := json.RawMessage(`{"model":"claude-4"}`)
	if err := c.ProxyUpdateConfig(context.Background(), "my-proj", cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClient_ProxyUpdateConfig_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if err := c.ProxyUpdateConfig(context.Background(), "proj", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error on 403")
	}
}

// ---- ProxyRemoveSession ----

func TestHTTPClient_ProxyRemoveSession_ok(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/sessions" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	removed, err := c.ProxyRemoveSession(context.Background(), "feishu:group:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !removed {
		t.Fatal("expected removed=true on 200")
	}
	if gotBody["key"] != "feishu:group:abc" {
		t.Errorf("expected key in body, got %v", gotBody)
	}
}

func TestHTTPClient_ProxyRemoveSession_notFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	removed, err := c.ProxyRemoveSession(context.Background(), "k")
	if err != nil {
		t.Fatalf("expected nil err on 404, got %v", err)
	}
	if removed {
		t.Fatal("expected removed=false on 404")
	}
}

func TestHTTPClient_ProxyRemoveSession_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if _, err := c.ProxyRemoveSession(context.Background(), "k"); err == nil {
		t.Fatal("expected error on 500")
	}
}

// ---- ProxyInterruptSession ----

func TestHTTPClient_ProxyInterruptSession_ok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/interrupt" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	interrupted, err := c.ProxyInterruptSession(context.Background(), "k")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !interrupted {
		t.Fatal("expected interrupted=true on status=ok")
	}
}

func TestHTTPClient_ProxyInterruptSession_notRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "not_running"})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	interrupted, err := c.ProxyInterruptSession(context.Background(), "k")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if interrupted {
		t.Fatal("expected interrupted=false when remote reports not_running")
	}
}

func TestHTTPClient_ProxyInterruptSession_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if _, err := c.ProxyInterruptSession(context.Background(), "k"); err == nil {
		t.Fatal("expected error on 502")
	}
}

// ---- ProxySetSessionLabel ----

func TestHTTPClient_ProxySetSessionLabel_ok(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/sessions/label" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "label": gotBody["label"]})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	updated, err := c.ProxySetSessionLabel(context.Background(), "feishu:direct:alice:general", "my-label")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !updated {
		t.Fatal("expected updated=true on 200")
	}
	if gotBody["key"] != "feishu:direct:alice:general" {
		t.Errorf("key in body = %q, want feishu:direct:alice:general", gotBody["key"])
	}
	if gotBody["label"] != "my-label" {
		t.Errorf("label in body = %q, want my-label", gotBody["label"])
	}
}

func TestHTTPClient_ProxySetSessionLabel_notFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	updated, err := c.ProxySetSessionLabel(context.Background(), "k", "x")
	if err != nil {
		t.Fatalf("expected nil err on 404, got %v", err)
	}
	if updated {
		t.Fatal("expected updated=false on 404")
	}
}

func TestHTTPClient_ProxySetSessionLabel_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if _, err := c.ProxySetSessionLabel(context.Background(), "k", "x"); err == nil {
		t.Fatal("expected error on 500")
	}
}

// ---- Metadata accessors ----

func TestHTTPClient_Accessors(t *testing.T) {
	c := NewHTTPClient("id1", "http://example.com", "tok", "My Node")
	if c.NodeID() != "id1" {
		t.Errorf("NodeID: want %q, got %q", "id1", c.NodeID())
	}
	if c.DisplayName() != "My Node" {
		t.Errorf("DisplayName: want %q, got %q", "My Node", c.DisplayName())
	}
	if c.Status() != "ok" {
		t.Errorf("Status: want %q, got %q", "ok", c.Status())
	}
	if c.RemoteAddr() != "http://example.com" {
		t.Errorf("RemoteAddr: want %q, got %q", "http://example.com", c.RemoteAddr())
	}
}

// ---- Context cancellation ----

func TestHTTPClient_FetchSessions_contextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow server
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := c.FetchSessions(ctx)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// ---- RefreshSubscription no-op ----

func TestHTTPClient_RefreshSubscription_noop(t *testing.T) {
	c := NewHTTPClient("id", "http://localhost", "", "")
	// Should not panic or do anything.
	c.RefreshSubscription("some-key")
}

// ---- Close idempotent ----

func TestHTTPClient_Close_idempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"sessions": []any{}})
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	// Close before any relay is created — should be safe.
	c.Close()
	c.Close() // second close must not panic
}
