package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsLoopbackRemote_PerAddrShape pins the host-parse semantics the
// pprof defense-in-depth check relies on. Covers the three shapes
// Go's stdlib http.Server can produce for RemoteAddr (ipv4:port,
// bracketed-ipv6:port, bare ip after proxy strip) plus a handful of
// obvious non-loopback cases that must be rejected.
func TestIsLoopbackRemote_PerAddrShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr string
		want bool
	}{
		// Loopback — must pass
		{"127.0.0.1:55555", true},
		{"127.1.2.3:8080", true}, // full 127/8 is loopback per RFC 5735
		{"[::1]:8080", true},
		{"::1", true},
		{"127.0.0.1", true},

		// Not loopback — must fail
		{"10.0.0.5:8080", false},
		{"192.168.1.1:80", false},
		{"[2001:db8::1]:80", false},
		{"8.8.8.8:443", false},

		// Garbage — must fail (fail-closed)
		{"", false},
		{"not-an-ip", false},
		{"localhost:8080", false}, // hostname never resolves in this helper by design
		{"::garbage", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			if got := isLoopbackRemote(tc.addr); got != tc.want {
				t.Errorf("isLoopbackRemote(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestPprof_RequiresAuth pins that the pprof subtree sits behind the
// same requireAuth middleware as every other /api/* route. An
// unauthenticated GET must return 401, never a profile body. This is
// the defense-in-depth layer that catches "someone ran naozhi without
// a dashboard_token" — without it the loopback gate alone would let
// anyone on the host pull profiles.
func TestPprof_RequiresAuth(t *testing.T) {
	t.Parallel()
	srv := newPprofTestServer(t, "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/api/debug/pprof/", nil)
	r.RemoteAddr = "127.0.0.1:55555" // loopback — still must be rejected without auth
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/debug/pprof/ → status %d, want 401; body=%q",
			w.Code, w.Body.String())
	}
	// The 401 response body must NOT leak pprof UI or process metadata.
	body := w.Body.String()
	if strings.Contains(strings.ToLower(body), "pprof") {
		t.Errorf("401 body leaks pprof text: %q", body)
	}
}

// TestPprof_RejectsNonLoopback pins that authenticated requests from a
// non-loopback remote address are still refused. This is the layer
// that contains damage when the dashboard token leaks or the ALB
// terminates without stripping Authorization.
func TestPprof_RejectsNonLoopback(t *testing.T) {
	t.Parallel()
	srv := newPprofTestServer(t, "secret-token")

	tests := []string{
		"10.0.0.5:40001",      // internal subnet
		"192.168.1.100:40001", // RFC1918
		"[2001:db8::1]:40001", // public IPv6 doc range
		"203.0.113.10:40001",  // public IPv4 doc range
	}
	for _, remote := range tests {
		remote := remote
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/api/debug/pprof/", nil)
			r.RemoteAddr = remote
			r.Header.Set("Authorization", "Bearer secret-token")
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Fatalf("remote=%s authed but not loopback: status=%d, want 403", remote, w.Code)
			}
			if !strings.Contains(w.Body.String(), "loopback-only") {
				t.Errorf("403 body should name the loopback requirement so operators know what's wrong; got %q",
					w.Body.String())
			}
		})
	}
}

// TestPprof_LoopbackAuthenticatedServesIndex pins the happy path:
// authenticated request from 127.0.0.1 returns the pprof HTML index.
// The body must name at least one known profile so this test would
// catch a completely bungled handler wiring (e.g. forgetting to
// strip the /api prefix and falling through to 404).
func TestPprof_LoopbackAuthenticatedServesIndex(t *testing.T) {
	t.Parallel()
	srv := newPprofTestServer(t, "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/api/debug/pprof/", nil)
	r.RemoteAddr = "127.0.0.1:55555"
	r.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("loopback+auth GET /api/debug/pprof/ → %d, want 200; body=%q",
			w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The stdlib pprof Index renders a table of profile links — the
	// "heap" profile is always registered so its name must appear.
	// If this regresses, the prefix-stripping logic is probably wrong.
	if !strings.Contains(body, "heap") {
		t.Errorf("pprof index body missing 'heap' link — prefix strip likely broken; got prefix=%q",
			truncate(body, 200))
	}
}

// TestPprof_LoopbackAuthenticatedNamedProfile pins that named
// profile paths (e.g. /api/debug/pprof/goroutine) route to pprof's
// per-profile handler and return a non-empty response. Uses
// ?debug=1 so the response is text-formatted (not gzipped binary)
// for a cheap body sanity check.
func TestPprof_LoopbackAuthenticatedNamedProfile(t *testing.T) {
	t.Parallel()
	srv := newPprofTestServer(t, "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/api/debug/pprof/goroutine?debug=1", nil)
	r.RemoteAddr = "127.0.0.1:55555"
	r.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("named profile goroutine → status %d, want 200", w.Code)
	}
	body := w.Body.String()
	// goroutine?debug=1 output starts with "goroutine profile:" header.
	if !strings.Contains(body, "goroutine profile") {
		t.Errorf("goroutine profile body unexpected shape; head=%q", truncate(body, 120))
	}
}

// newPprofTestServer builds a minimal Server with just enough wiring
// for the pprof handlers + auth middleware to operate. Skips the
// full router/hub initialization because pprof doesn't touch them.
func newPprofTestServer(t *testing.T, token string) *Server {
	t.Helper()
	auth := &AuthHandlers{
		dashboardToken: token,
		cookieSecret:   []byte("test-cookie-secret"),
		loginLimiter:   newLoginLimiter(),
	}
	s := &Server{
		mux:  http.NewServeMux(),
		auth: auth,
	}
	s.registerPprof()
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
