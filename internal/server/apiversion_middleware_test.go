package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWithAPIVersionAlias_RewritesV1ToUnversioned pins RNEW-ARCH-401 (#425):
// a `/api/v1/<rest>` request must reach the handler registered at
// `/api/<rest>`, and unrelated / unversioned paths must pass through verbatim.
func TestWithAPIVersionAlias_RewritesV1ToUnversioned(t *testing.T) {
	cases := []struct {
		name    string
		reqPath string
		want    string // path the downstream handler observes
	}{
		{"v1 sessions -> unversioned", "/api/v1/sessions", "/api/sessions"},
		{"v1 nested -> unversioned", "/api/v1/cron/runs/abc", "/api/cron/runs/abc"},
		{"v1 trailing segment only", "/api/v1/health", "/api/health"},
		{"unversioned untouched", "/api/sessions", "/api/sessions"},
		{"non-api untouched", "/dashboard", "/dashboard"},
		{"ws untouched", "/ws", "/ws"},
		{"static untouched", "/static/dashboard.js", "/static/dashboard.js"},
		// `/api/v1` with no trailing slash is NOT the alias prefix; leave it so
		// the mux returns its normal 404 instead of a surprising rewrite.
		{"api/v1 no trailing slash untouched", "/api/v1", "/api/v1"},
		// A bare `/api/v1/` (empty rest) maps to `/api/` — harmless; the mux
		// has no `/api/` route so it 404s, but the rewrite must be deterministic.
		{"api/v1/ empty rest", "/api/v1/", "/api/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
			})
			h := withAPIVersionAlias(next)

			req := httptest.NewRequest(http.MethodGet, tc.reqPath, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if seen != tc.want {
				t.Fatalf("downstream path = %q, want %q", seen, tc.want)
			}
		})
	}
}

// TestWithAPIVersionAlias_PreservesOriginalRequestURI asserts that only
// r.URL.Path (what the mux matches) is rewritten; RequestURI keeps the
// version the client actually called so logging / trace middleware can record
// it. RNEW-ARCH-401 (#425).
func TestWithAPIVersionAlias_PreservesOriginalRequestURI(t *testing.T) {
	var seenPath, seenURI string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenURI = r.RequestURI
	})
	h := withAPIVersionAlias(next)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seenPath != "/api/sessions" {
		t.Fatalf("URL.Path = %q, want /api/sessions", seenPath)
	}
	if seenURI != "/api/v1/sessions" {
		t.Fatalf("RequestURI = %q, want /api/v1/sessions (original must survive for logging)", seenURI)
	}
}

// TestWithAPIVersionAlias_V1RouteResolvesThroughRealMux is an end-to-end pin:
// register a handler at the unversioned path on a real ServeMux and confirm
// the alias makes the same handler reachable via `/api/v1/...`. This is the
// behaviour external node / IM consumers depend on. RNEW-ARCH-401 (#425).
func TestWithAPIVersionAlias_V1RouteResolvesThroughRealMux(t *testing.T) {
	mux := http.NewServeMux()
	hit := false
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	})
	h := withAPIVersionAlias(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !hit {
		t.Fatal("/api/v1/ping did not resolve to the /api/ping handler")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}
