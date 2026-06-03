package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPprof_DisabledWhenNoToken pins #1633: with an empty DashboardToken,
// RequireAuth's IsAuthenticated returns true for every caller, so the only
// remaining gate is loopback. A compromised on-host subprocess could then
// pull goroutine/heap dumps unauthenticated. The handler must instead return
// 503 and serve no profile body, even from a loopback address.
func TestPprof_DisabledWhenNoToken(t *testing.T) {
	t.Parallel()
	srv := newPprofTestServer(t, "") // no dashboard_token

	r := httptest.NewRequest(http.MethodGet, "/api/debug/pprof/", nil)
	r.RemoteAddr = "127.0.0.1:55555" // loopback — still must be refused
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-token loopback GET /api/debug/pprof/ → %d, want 503; body=%q",
			w.Code, w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "heap") {
		t.Errorf("503 body leaks pprof index content: %q", w.Body.String())
	}
}

// TestExpvar_DisabledWhenNoToken pins the same #1633 posture for expvar:
// no dashboard_token => 503, no counters served, even on loopback.
func TestExpvar_DisabledWhenNoToken(t *testing.T) {
	t.Parallel()
	srv := newExpvarTestServer(t, "") // no dashboard_token

	r := httptest.NewRequest(http.MethodGet, "/api/debug/vars", nil)
	r.RemoteAddr = "127.0.0.1:55555"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-token loopback GET /api/debug/vars → %d, want 503; body=%q",
			w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "goroutines") {
		t.Errorf("503 body leaks expvar content: %q", w.Body.String())
	}
}
