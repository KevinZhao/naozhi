package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	clipkg "github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

// fakeConn is a node.Conn stub whose only exercised method is FetchBackends.
// Every other method is a no-op — the ?node= proxy path in Handle only calls
// FetchBackends, so we keep the surface minimal (mirrors cache_test.go's
// stubConn but local to this package).
type fakeConn struct {
	raw json.RawMessage
	err error
}

func (f *fakeConn) FetchBackends(_ context.Context) (json.RawMessage, error) {
	return f.raw, f.err
}

// --- node.Conn no-op filler methods -------------------------------------
func (f *fakeConn) NodeID() string       { return "remote" }
func (f *fakeConn) DisplayName() string  { return "remote" }
func (f *fakeConn) RemoteAddr() string   { return "fake" }
func (f *fakeConn) Status() string       { return "ok" }
func (f *fakeConn) Meta() *node.NodeMeta { return &node.NodeMeta{NodeID: "remote"} }
func (f *fakeConn) Close()               {}
func (f *fakeConn) FetchSessions(_ context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (f *fakeConn) FetchProjects(_ context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (f *fakeConn) FetchDiscovered(_ context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (f *fakeConn) FetchDiscoveredPreview(_ context.Context, _ string) ([]clipkg.EventEntry, error) {
	return nil, nil
}
func (f *fakeConn) FetchEvents(_ context.Context, _ string, _ int64) ([]clipkg.EventEntry, error) {
	return nil, nil
}
func (f *fakeConn) Send(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeConn) ProxyTakeover(_ context.Context, _ int, _, _ string, _ uint64) (string, error) {
	return "", nil
}
func (f *fakeConn) ProxyCloseDiscovered(_ context.Context, _ int, _, _ string, _ uint64) error {
	return nil
}
func (f *fakeConn) ProxyRestartPlanner(_ context.Context, _ string) error { return nil }
func (f *fakeConn) ProxyUpdateConfig(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}
func (f *fakeConn) ProxySetFavorite(_ context.Context, _ string, _ bool) error { return nil }
func (f *fakeConn) ProxyRemoveSession(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (f *fakeConn) ProxyInterruptSession(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (f *fakeConn) ProxySetSessionLabel(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
func (f *fakeConn) Subscribe(_ node.EventSink, _ string, _ int64) {}
func (f *fakeConn) Unsubscribe(_ node.EventSink, _ string)        {}
func (f *fakeConn) RefreshSubscription(_ string)                  {}
func (f *fakeConn) RemoveClient(_ node.EventSink)                 {}

// fakeNodeAccess resolves a single node id to conn. When conn is nil,
// LookupNode writes a 404 (mirroring the real nodeAccessor's "unknown node"
// response) and returns false.
type fakeNodeAccess struct {
	wantID string
	conn   node.Conn
}

func (a *fakeNodeAccess) LookupNode(w http.ResponseWriter, id string) (node.Conn, bool) {
	if a.conn == nil || id != a.wantID {
		http.Error(w, "node not found", http.StatusNotFound)
		return nil, false
	}
	return a.conn, true
}

// TestHandle_NodeProxy_RelaysRemoteManifest pins the picker node-aware fix:
// GET /api/cli/backends?node=<remote> must proxy to that node's FetchBackends
// and relay its manifest verbatim — the remote node's backends + default, NOT
// the local router's. Without this the dashboard picker pre-selected the
// primary's default on a remote-targeted new session.
func TestHandle_NodeProxy_RelaysRemoteManifest(t *testing.T) {
	// Local router advertises claude as default; the remote node advertises a
	// DIFFERENT default (kiro) so a leak of the local manifest is detectable.
	r, _ := routerWithWrapper("2.1.100")
	remoteBody := `{"backends":[{"id":"kiro","available":true}],"default":"kiro","detected":[]}`
	h := &Handler{router: r}
	h.SetNodeAccess(&fakeNodeAccess{wantID: "remote", conn: &fakeConn{raw: json.RawMessage(remoteBody)}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cli/backends?node=remote", nil)
	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Default  string `json:"default"`
		Backends []struct {
			ID string `json:"id"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Default != "kiro" {
		t.Fatalf("default = %q, want remote node's %q (local manifest leaked?)", body.Default, "kiro")
	}
	if len(body.Backends) != 1 || body.Backends[0].ID != "kiro" {
		t.Fatalf("backends = %+v, want remote node's [kiro]", body.Backends)
	}
}

// TestHandle_NodeProxy_LocalFallsThrough confirms ?node=local (and empty node)
// bypasses the proxy and serves the local router manifest.
func TestHandle_NodeProxy_LocalFallsThrough(t *testing.T) {
	r, _ := routerWithWrapper("2.1.100")
	h := &Handler{router: r}
	// nodeAccess deliberately resolves nothing — a local request must never
	// consult it.
	h.SetNodeAccess(&fakeNodeAccess{wantID: "never", conn: nil})

	for _, target := range []string{"/api/cli/backends", "/api/cli/backends?node=local"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		h.Handle(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", target, rec.Code)
		}
		got, ok := findBackend(decodeBackendsFrom(t, rec.Body.Bytes()), "claude")
		if !ok || got.ID != "claude" {
			t.Fatalf("%s: local manifest missing claude backend", target)
		}
	}
}

// TestHandle_NodeProxy_UpstreamErrorIs502 pins the degrade contract: when the
// remote FetchBackends errors (e.g. an older peer that predates fetch_backends
// returns an unknown-method error), Handle returns 502 so the dashboard's
// fetch falls back to the local manifest rather than silently showing a wrong
// list.
func TestHandle_NodeProxy_UpstreamErrorIs502(t *testing.T) {
	r, _ := routerWithWrapper("2.1.100")
	h := &Handler{router: r}
	h.SetNodeAccess(&fakeNodeAccess{wantID: "remote", conn: &fakeConn{err: errors.New("unknown method: fetch_backends")}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cli/backends?node=remote", nil)
	h.Handle(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on upstream error", rec.Code)
	}
}

// TestHandle_NodeProxy_NoAccessorIs502 covers a single-node build (nodeAccess
// never wired): a stray ?node= request degrades to 502 rather than panicking
// on a nil accessor.
func TestHandle_NodeProxy_NoAccessorIs502(t *testing.T) {
	r, _ := routerWithWrapper("2.1.100")
	h := &Handler{router: r} // SetNodeAccess never called → nodeAccess nil

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cli/backends?node=remote", nil)
	h.Handle(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when node routing unavailable", rec.Code)
	}
}

// TestHandle_NodeProxy_UnknownNodePropagates confirms LookupNode's own error
// response (404 for an unknown node id) is not overwritten — Handle must
// return after LookupNode already wrote to w.
func TestHandle_NodeProxy_UnknownNodePropagates(t *testing.T) {
	r, _ := routerWithWrapper("2.1.100")
	h := &Handler{router: r}
	h.SetNodeAccess(&fakeNodeAccess{wantID: "known", conn: &fakeConn{raw: json.RawMessage(`{}`)}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cli/backends?node=ghost", nil)
	h.Handle(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 from LookupNode for unknown node", rec.Code)
	}
}

// decodeBackendsFrom parses a "backends" list out of an already-captured
// response body (the proxy tests can't reuse decodeBackends, which re-runs the
// handler with a fresh local request).
func decodeBackendsFrom(t *testing.T, raw []byte) []clipkg.BackendInfo {
	t.Helper()
	var body struct {
		Backends []clipkg.BackendInfo `json:"backends"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body.Backends
}
