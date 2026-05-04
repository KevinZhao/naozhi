package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExpvar_RequiresAuth pins that /api/debug/vars sits behind requireAuth.
// An unauthenticated GET from loopback must still return 401 — the auth
// layer enforces who can read; the loopback gate enforces where from.
func TestExpvar_RequiresAuth(t *testing.T) {
	t.Parallel()
	srv := newExpvarTestServer(t, "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/api/debug/vars", nil)
	r.RemoteAddr = "127.0.0.1:55555"
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/debug/vars → status %d, want 401; body=%q",
			w.Code, w.Body.String())
	}
	// 401 body must not leak the JSON counter dump.
	if strings.Contains(w.Body.String(), "naozhi_") {
		t.Errorf("401 body leaks expvar counter names: %q", w.Body.String())
	}
}

// TestExpvar_RejectsNonLoopback pins the loopback-only gate. trustedProxy
// mode is NOT exempt: a compromised ALB must not be able to smuggle
// expvar out via forged X-Forwarded-For.
func TestExpvar_RejectsNonLoopback(t *testing.T) {
	t.Parallel()
	srv := newExpvarTestServer(t, "secret-token")

	for _, remote := range []string{
		"10.0.0.5:40001",
		"192.168.1.100:40001",
		"[2001:db8::1]:40001",
		"203.0.113.10:40001",
	} {
		remote := remote
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/api/debug/vars", nil)
			r.RemoteAddr = remote
			r.Header.Set("Authorization", "Bearer secret-token")
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Fatalf("remote=%s: status=%d, want 403", remote, w.Code)
			}
			if !strings.Contains(w.Body.String(), "loopback-only") {
				t.Errorf("403 body should name the loopback requirement; got %q",
					w.Body.String())
			}
		})
	}
}

// TestExpvar_LoopbackAuthenticatedServesJSON pins the happy path: the
// response is a JSON object that contains all five naozhi_* counters
// plus stdlib's cmdline/memstats. If any counter is missing this test
// catches an init-order regression (e.g. a counter moved to a package
// that is not linked into naozhi binary).
func TestExpvar_LoopbackAuthenticatedServesJSON(t *testing.T) {
	t.Parallel()
	srv := newExpvarTestServer(t, "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/api/debug/vars", nil)
	r.RemoteAddr = "127.0.0.1:55555"
	r.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("loopback+auth GET /api/debug/vars → %d, want 200; body=%q",
			w.Code, truncate(w.Body.String(), 200))
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expvar response is not valid JSON: %v; body=%q", err, truncate(w.Body.String(), 200))
	}
	// Operator runbook depends on all five names being in the payload.
	for _, want := range []string{
		"naozhi_session_create_total",
		"naozhi_session_evict_total",
		"naozhi_cli_spawn_total",
		"naozhi_ws_auth_fail_total",
		"naozhi_shim_restart_total",
	} {
		if _, ok := payload[want]; !ok {
			t.Errorf("expvar payload missing counter %q (present keys sample: %v)",
				want, sampleKeys(payload, 10))
		}
	}
	// Stdlib counters must also be present — they confirm we wired up
	// expvar.Handler() rather than a naked naozhi-only dump.
	for _, want := range []string{"cmdline", "memstats"} {
		if _, ok := payload[want]; !ok {
			t.Errorf("expvar payload missing stdlib var %q", want)
		}
	}
}

func newExpvarTestServer(t *testing.T, token string) *Server {
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
	s.registerExpvar()
	return s
}

func sampleKeys(m map[string]any, n int) []string {
	out := make([]string, 0, n)
	for k := range m {
		out = append(out, k)
		if len(out) == n {
			break
		}
	}
	return out
}
