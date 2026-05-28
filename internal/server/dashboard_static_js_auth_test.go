package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardJS_RequiresAuth_TokenMode pins R20260527122801-SEC-4
// (#1328) and R249-SEC-10 (#923): in token mode, GET /static/dashboard.js
// MUST return 401 to unauthenticated callers AND the 401 body MUST NOT
// leak any JS source. The previous mux entry served the JS module to
// anyone, leaking the API endpoint list, cron polling cadence, and
// client-side schema to scanners — a free recon surface that the
// dashboard editor has no functional reason to expose pre-login.
func TestDashboardJS_RequiresAuth_TokenMode(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithToken(&mockPlatform{}, "secret")

	req := httptest.NewRequest(http.MethodGet, "/static/dashboard.js", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/static/dashboard.js unauth GET = %d, want 401 (regression of SEC-4 #1328: scanner can fingerprint deployment via JS body)", w.Code)
	}
	// #923: even on 401, ensure no JS source bytes leak into the body.
	// The body should be the plain "unauthorized" / empty 401 response,
	// not a slice of the embedded asset.
	body := w.Body.String()
	for _, leak := range []string{"PLATFORM_ORIGINS", "function ", "/api/sessions"} {
		if strings.Contains(body, leak) {
			t.Errorf("#923 regression: 401 body leaks JS source token %q (would let scanner fingerprint deployment)", leak)
		}
	}
}

// TestAgentViewJS_RequiresAuth_TokenMode is the companion guard for the
// other JS module that ships dashboard internals. Same regression class
// (#1328 / #923).
func TestAgentViewJS_RequiresAuth_TokenMode(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithToken(&mockPlatform{}, "secret")

	req := httptest.NewRequest(http.MethodGet, "/static/agent_view.js", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/static/agent_view.js unauth GET = %d, want 401 (regression of SEC-4 #1328)", w.Code)
	}
	// #923: 401 body must not leak source bytes from agent_view.js.
	body := w.Body.String()
	for _, leak := range []string{"function ", "agentView", "subagent"} {
		if strings.Contains(body, leak) {
			t.Errorf("#923 regression: 401 body leaks agent_view.js source token %q", leak)
		}
	}
}

// TestDashboardJS_NoTokenModeStillServes pins the no-token-mode escape
// valve: requireAuth is a pass-through when dashboardToken == "" (single-
// user / loopback deployments), so the JS module still loads on a fresh
// request without any auth header. This is the legacy contract the SEC-4
// fix must NOT break — a no-token operator has no login page in front of
// the dashboard, and the JS file load is part of the bootstrap.
func TestDashboardJS_NoTokenModeStillServes(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{}) // no token -> no-token mode

	req := httptest.NewRequest(http.MethodGet, "/static/dashboard.js", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/static/dashboard.js no-token GET = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

// TestAgentViewJS_NoTokenModeStillServes mirrors the no-token contract
// for the agent-team-ui module so the same escape valve is locked.
func TestAgentViewJS_NoTokenModeStillServes(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/static/agent_view.js", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/static/agent_view.js no-token GET = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}
