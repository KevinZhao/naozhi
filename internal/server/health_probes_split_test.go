package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
