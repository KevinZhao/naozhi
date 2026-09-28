package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleLivez_AlwaysOK locks the contract that /livez never depends on
// router / hub / CLI state — it is the K8s liveness probe and a non-200
// triggers a restart loop. Even when handleHealth would refuse fields
// (router stats etc.), /livez must return 200.
//
// Anti-regression: a future refactor that adds a dep check inside
// handleLivez would turn a transient eventlog drain wobble into a kill;
// this test fails in that case.
func TestHandleLivez_AlwaysOK(t *testing.T) {
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	w := httptest.NewRecorder()
	hs.healthH.handleLivez(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "ok" {
		t.Errorf("body = %q, want \"ok\"", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q, want no-store", cc)
	}
}

// TestHandleReadyz_OKWhenWired returns 200 ready when router is wired —
// the standard happy path on a normally-started server.
func TestHandleReadyz_OKWhenWired(t *testing.T) {
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	hs.healthH.handleReadyz(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "ready" {
		t.Errorf("body = %q, want \"ready\"", got)
	}
}

// TestHandleReadyz_FailsWhenRouterNil verifies the readiness gate fails
// closed when the constructor-level wiring is incomplete — operators
// must see a 503 (LB withdraws traffic) rather than a misleading 200.
func TestHandleReadyz_FailsWhenRouterNil(t *testing.T) {
	h := &HealthHandler{router: nil}
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.handleReadyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not ready") {
		t.Errorf("body = %q, want contains \"not ready\"", w.Body.String())
	}
}

// TestHandleLivez_NoAuthRequired confirms /livez bypasses the auth gate —
// the K8s liveness probe carries no credentials and a 401/403 here would
// trigger a restart loop the same way a 5xx would.
func TestHandleLivez_NoAuthRequired(t *testing.T) {
	_, hs := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	// No Authorization header.
	w := httptest.NewRecorder()
	hs.healthH.handleLivez(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("unauth status = %d, want 200", w.Code)
	}
}

// TestHealthProbes_RoutedToTheirHandlers drives each probe through the mux:
// the other health tests call the handler methods directly, so they cannot
// see a route mounted on the wrong one.
func TestHealthProbes_RoutedToTheirHandlers(t *testing.T) {
	srv, _ := newTestServerWithTokenHS(&mockPlatform{}, "secret")
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	if w := get("/health"); w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), `"status"`) {
		t.Errorf("GET /health = %d %q %q, want the JSON health body", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if w := get("/livez"); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "ok" {
		t.Errorf("GET /livez = %d %q, want 200 ok", w.Code, w.Body.String())
	}
	if w := get("/readyz"); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "ready" {
		t.Errorf("GET /readyz = %d %q, want 200 ready", w.Code, w.Body.String())
	}
}
