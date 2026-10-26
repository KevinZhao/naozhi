package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

// ---- FetchBackends ----

func TestHTTPClient_FetchBackends_relaysVerbatim(t *testing.T) {
	// The manifest is relayed as opaque bytes; assert the exact body is
	// passed through untouched (no decode→re-encode reshaping).
	want := `{"backends":[{"id":"kiro","available":true}],"default":"kiro","detected":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/cli/backends" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(want))
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "tok")
	got, err := c.FetchBackends(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != want {
		t.Fatalf("relayed body = %q, want verbatim %q", string(got), want)
	}
}

func TestHTTPClient_FetchBackends_errorStatus(t *testing.T) {
	// A peer predating the endpoint 404s; the caller must see an error so it
	// degrades to the local manifest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if _, err := c.FetchBackends(context.Background()); err == nil {
		t.Fatal("expected error on 404 status")
	}
}

func TestHTTPClient_FetchBackends_malformedJSON(t *testing.T) {
	// A compromised/buggy peer must not let us relay non-JSON bytes under an
	// application/json content type.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "")
	if _, err := c.FetchBackends(context.Background()); err == nil {
		t.Fatal("expected error on malformed JSON body")
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

// ---- Per-RPC request shape and result ----

// httpRPCCase is one HTTPClient method: the request it must send and how a
// successful reply maps to its return value.
type httpRPCCase struct {
	name   string
	method string
	path   string
	query  map[string]string
	body   map[string]any // nil: no body expected
	status int            // reply status on success; 0 means 200
	reply  string
	call   func(*HTTPClient) (any, error)
	want   any
}

type seenRequest struct {
	method, path, query string
	body                map[string]any
}

func httpRPCCases() []httpRPCCase {
	ctx := context.Background()
	return []httpRPCCase{
		{
			name: "FetchSessions", method: http.MethodGet, path: "/api/sessions",
			reply: `{"sessions":[{"session_id":"abc","state":"idle"},{"session_id":"def","state":"busy"}]}`,
			call:  func(c *HTTPClient) (any, error) { s, err := c.FetchSessions(ctx); return len(s), err },
			want:  2,
		},
		{
			name: "FetchEvents", method: http.MethodGet, path: "/api/sessions/events",
			query: map[string]string{"key": "feishu:group:123"},
			reply: `[{"time":1000,"type":"text","summary":"hello"}]`,
			call: func(c *HTTPClient) (any, error) {
				e, err := c.FetchEvents(ctx, "feishu:group:123", 0)
				return len(e), err
			},
			want: 1,
		},
		{
			name: "FetchEvents after", method: http.MethodGet, path: "/api/sessions/events",
			query: map[string]string{"key": "k", "after": "9999"},
			reply: `[]`,
			call:  func(c *HTTPClient) (any, error) { e, err := c.FetchEvents(ctx, "k", 9999); return len(e), err },
			want:  0,
		},
		{
			// A 200 with no body is a node that took the send.
			name: "Send", method: http.MethodPost, path: "/api/sessions/send",
			body: map[string]any{"key": "feishu:group:123", "text": "hi"},
			call: func(c *HTTPClient) (any, error) { return c.Send(ctx, "feishu:group:123", "hi", "") },
			want: "accepted",
		},
		{
			name: "Send workspace", method: http.MethodPost, path: "/api/sessions/send",
			body:   map[string]any{"key": "k", "text": "text", "workspace": "/home/user/project"},
			status: http.StatusAccepted,
			call:   func(c *HTTPClient) (any, error) { return c.Send(ctx, "k", "text", "/home/user/project") },
			want:   "accepted",
		},
		{
			name: "FetchProjects", method: http.MethodGet, path: "/api/projects",
			reply: `[{"name":"proj1"}]`,
			call:  func(c *HTTPClient) (any, error) { p, err := c.FetchProjects(ctx); return len(p), err },
			want:  1,
		},
		{
			name: "FetchDiscovered", method: http.MethodGet, path: "/api/discovered",
			reply: `[{"pid":12345}]`,
			call:  func(c *HTTPClient) (any, error) { d, err := c.FetchDiscovered(ctx); return len(d), err },
			want:  1,
		},
		{
			name: "FetchDiscoveredPreview", method: http.MethodGet, path: "/api/discovered/preview",
			query: map[string]string{"session_id": "sess-abc"},
			reply: `[{"time":1,"type":"text"}]`,
			call: func(c *HTTPClient) (any, error) {
				e, err := c.FetchDiscoveredPreview(ctx, "sess-abc")
				return len(e), err
			},
			want: 1,
		},
		{
			name: "ProxyTakeover", method: http.MethodPost, path: "/api/discovered/takeover",
			body:  map[string]any{"pid": 42.0, "session_id": "sess-1", "cwd": "/cwd", "proc_start_time": 12345.0},
			reply: `{"key":"feishu:group:123"}`,
			call:  func(c *HTTPClient) (any, error) { return c.ProxyTakeover(ctx, 42, "sess-1", "/cwd", 12345) },
			want:  "feishu:group:123",
		},
		{
			name: "ProxyCloseDiscovered", method: http.MethodPost, path: "/api/discovered/close",
			body: map[string]any{"pid": 42.0, "session_id": "sess-1", "cwd": "/", "proc_start_time": 0.0},
			call: func(c *HTTPClient) (any, error) { return nil, c.ProxyCloseDiscovered(ctx, 42, "sess-1", "/", 0) },
		},
		{
			name: "ProxyUpdateConfig", method: http.MethodPut, path: "/api/projects/config",
			query: map[string]string{"name": "my-proj"},
			body:  map[string]any{"model": "claude-4"},
			call: func(c *HTTPClient) (any, error) {
				return nil, c.ProxyUpdateConfig(ctx, "my-proj", json.RawMessage(`{"model":"claude-4"}`))
			},
		},
		{
			name: "ProxyRemoveSession", method: http.MethodDelete, path: "/api/sessions",
			body: map[string]any{"key": "feishu:group:abc"},
			call: func(c *HTTPClient) (any, error) { return c.ProxyRemoveSession(ctx, "feishu:group:abc") },
			want: true,
		},
		{
			name: "ProxyInterruptSession", method: http.MethodPost, path: "/api/sessions/interrupt",
			body:  map[string]any{"key": "k"},
			reply: `{"status":"ok"}`,
			call:  func(c *HTTPClient) (any, error) { return c.ProxyInterruptSession(ctx, "k") },
			want:  true,
		},
		{
			name: "ProxySetSessionLabel", method: http.MethodPatch, path: "/api/sessions/label",
			body:  map[string]any{"key": "feishu:direct:alice:general", "label": "my-label"},
			reply: `{"status":"ok","label":"my-label"}`,
			call: func(c *HTTPClient) (any, error) {
				return c.ProxySetSessionLabel(ctx, "feishu:direct:alice:general", "my-label")
			},
			want: true,
		},
	}
}

// TestHTTPClient_RPCs checks every method's HTTP verb, path, query and JSON
// body, then the value it decodes from a successful reply.
func TestHTTPClient_RPCs(t *testing.T) {
	for _, tc := range httpRPCCases() {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan seenRequest, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := seenRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query().Encode()}
				_ = json.NewDecoder(r.Body).Decode(&req.body)
				seen <- req
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer srv.Close()

			got, err := tc.call(newTestHTTPClient(t, srv, ""))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("result = %#v, want %#v", got, tc.want)
			}
			req := <-seen
			if req.method != tc.method || req.path != tc.path {
				t.Errorf("request = %s %s, want %s %s", req.method, req.path, tc.method, tc.path)
			}
			wantQuery := url.Values{}
			for k, v := range tc.query {
				wantQuery.Set(k, v)
			}
			if req.query != wantQuery.Encode() {
				t.Errorf("query = %q, want %q", req.query, wantQuery.Encode())
			}
			if !reflect.DeepEqual(req.body, tc.body) {
				t.Errorf("body = %v, want %v", req.body, tc.body)
			}
		})
	}
}

// TestHTTPClient_RPCs_errorStatus: each method checks the status code itself,
// so every one must turn a 500 into an error.
func TestHTTPClient_RPCs_errorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "")
	for _, tc := range httpRPCCases() {
		if _, err := tc.call(c); err == nil {
			t.Errorf("%s: expected error on 500", tc.name)
		}
	}
}

// TestHTTPClient_RPCs_notApplied: replies that mean "nothing to do" come back
// as false with no error.
func TestHTTPClient_RPCs_notApplied(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		status int
		reply  string
		call   func(*HTTPClient) (bool, error)
	}{
		{"ProxyRemoveSession 404", http.StatusNotFound, "not found", func(c *HTTPClient) (bool, error) { return c.ProxyRemoveSession(ctx, "k") }},
		{"ProxySetSessionLabel 404", http.StatusNotFound, "not found", func(c *HTTPClient) (bool, error) { return c.ProxySetSessionLabel(ctx, "k", "x") }},
		{"ProxyInterruptSession not_running", http.StatusOK, `{"status":"not_running"}`, func(c *HTTPClient) (bool, error) { return c.ProxyInterruptSession(ctx, "k") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer srv.Close()
			ok, err := tc.call(newTestHTTPClient(t, srv, ""))
			if err != nil || ok {
				t.Errorf("got (%v, %v), want (false, nil)", ok, err)
			}
		})
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

// TestHTTPClient_BlocksRedirects pins the R249-SEC-1 SSRF defence: every
// outbound request carries the dashboard Bearer token, so a 3xx from a
// compromised peer (or DNS/MITM) must surface to the caller instead of
// silently auto-following at IMDS / a loopback admin port. Mirrors the
// slack/feishu/discord/weixin contract tests.
func TestHTTPClient_BlocksRedirects(t *testing.T) {
	t.Parallel()
	c := NewHTTPClient("redir-test", "http://127.0.0.1:0", "secret", "Redir Test")
	if c.httpClient.CheckRedirect == nil {
		t.Fatal("HTTPClient.httpClient.CheckRedirect is nil — Bearer token can leak via 3xx (R249-SEC-1)")
	}
	if err := c.httpClient.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect returned %v, want http.ErrUseLastResponse", err)
	}
}
