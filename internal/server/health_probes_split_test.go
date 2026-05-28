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
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	w := httptest.NewRecorder()
	srv.healthH.handleLivez(w, req)

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
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	srv.healthH.handleReadyz(w, req)

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
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	// No Authorization header.
	w := httptest.NewRecorder()
	srv.healthH.handleLivez(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("unauth status = %d, want 200", w.Code)
	}
}
