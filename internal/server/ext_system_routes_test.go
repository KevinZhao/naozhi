package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Route-table tests for the /api/system/* group: the handlers live in
// dashboard/ext/system; what is pinned here is that routes.go wires them under
// the auth middleware with the right methods.

// The route must be registered as POST-only under the authenticated /api tree:
// GET on it would otherwise fall through to a 405-or-worse and, more to the
// point, an apply must never be reachable by a link or a prefetch.
func TestUpdateApply_RouteIsPostOnly(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	r := httptest.NewRequest(http.MethodGet, "/api/system/update/apply", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	if w.Code == http.StatusOK || w.Code == http.StatusAccepted {
		t.Errorf("GET on the apply route returned %d; it must be POST-only", w.Code)
	}
}

// Unauthenticated callers get 401 on both endpoints — the apply is behind the
// same dashboard token as everything else under /api.
func TestUpdateEndpoints_RequireAuth(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret-token")

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/system/update"},
		{http.MethodPost, "/api/system/update/apply"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"confirm_action":"install"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 without a token", tc.method, tc.path, w.Code)
		}
	}
}
