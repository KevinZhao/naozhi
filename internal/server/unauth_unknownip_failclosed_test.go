package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// newTestServerTrustedProxy builds a server with a dashboard token AND
// TrustedProxy=true so the unauthenticated rate-limit gates resolve the
// client IP from X-Forwarded-For rather than RemoteAddr.
func newTestServerTrustedProxy(p *mockPlatform, token string) *Server {
	s, _ := newTestServerTrustedProxyHS(p, token)
	return s
}

// newTestServerTrustedProxyHS is newTestServerTrustedProxy plus the handlerSet.
func newTestServerTrustedProxyHS(p *mockPlatform, token string) (*Server, *handlerSet) {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	return buildServerWithHandlers(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Platforms:      platforms,
		Backend:        "claude",
		DashboardToken: token,
		TrustedProxy:   true,
	})
}

// TestHandleHealth_TrustedProxy_MissingXFF_FailsClosed pins R20260614-SEC-10
// (#2120): in trusted-proxy mode an unauthenticated /health request with no
// X-Forwarded-For must be rejected on the FIRST request rather than sharing
// the unknownIPKey bucket. Pre-fix, clientIP() collapsed to "" → unknownIPKey,
// so a single direct-to-origin attacker shared one bucket with every other
// XFF-less probe (cross-tenant DoS amplifier). The gate must now fail closed,
// mirroring HandleLogin (R247-SEC-25).
//
// Regression shape: dropping the requestHasResolvableClientIP gate lets the
// first XFF-less request through (fresh unknownIPKey bucket has full budget),
// so a single 429-on-first-request assertion catches the revert.
func TestHandleHealth_TrustedProxy_MissingXFF_FailsClosed(t *testing.T) {
	_, hs := newTestServerTrustedProxyHS(&mockPlatform{}, "secret")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "10.0.0.1:1234" // proxy hop IP; no XFF appended
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("unauth /health with trustedProxy=true and missing XFF returned %d on first request, want 429 (fail-closed); the unknownIPKey shared-bucket regression is back", w.Code)
	}
}

// TestHandleHealth_TrustedProxy_ValidXFF_FirstAllowed confirms the negative:
// a parseable XFF resolves a real per-client IP and the first request is
// served (the gate is not just always-deny).
func TestHandleHealth_TrustedProxy_ValidXFF_FirstAllowed(t *testing.T) {
	_, hs := newTestServerTrustedProxyHS(&mockPlatform{}, "secret")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauth /health with trustedProxy=true and valid XFF returned %d on first request, want 200", w.Code)
	}
}

// TestHandleDashboard_TrustedProxy_MissingXFF_FailsClosed pins R20260614-SEC-10
// (#2120) for the unauthenticated GET /dashboard path (routes.go): an XFF-less
// request in trusted-proxy mode is refused up front rather than sharing
// unknownIPKey across every XFF-less scanner. The refusal is a 400 naming
// server.trusted_proxy, not a fake 429, because the usual caller is an
// operator opening the dashboard on the LAN (#3011).
func TestHandleDashboard_TrustedProxy_MissingXFF_FailsClosed(t *testing.T) {
	srv := newTestServerTrustedProxy(&mockPlatform{}, "secret")

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.RemoteAddr = "10.0.0.1:1234" // no XFF
		w := httptest.NewRecorder()
		srv.handleDashboard(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("request %d: unauth /dashboard with trustedProxy=true and missing XFF returned %d, want 400 (fail-closed with the reason)", i, w.Code)
		}
		body := w.Body.String()
		if body != auth.XFFRequiredReason+"\n" || !strings.Contains(body, "server.trusted_proxy") {
			t.Fatalf("request %d: body = %q, want the constant reason naming server.trusted_proxy", i, body)
		}
		if strings.Contains(body, "login-form") || w.Header().Get("Content-Security-Policy") != "" {
			t.Fatalf("request %d: the login page was served to an unresolvable client", i)
		}
	}
}

// TestHandleDashboard_UnauthLimiterExhausted_Is429 pins the other half of the
// split gate: a resolvable client that burns its unauthenticated budget still
// gets 429 with Retry-After, not the trusted_proxy reason.
func TestHandleDashboard_UnauthLimiterExhausted_Is429(t *testing.T) {
	srv := newTestServerTrustedProxy(&mockPlatform{}, "secret")

	for i := 0; i < 1000; i++ {
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		w := httptest.NewRecorder()
		srv.handleDashboard(w, req)
		if w.Code == http.StatusOK {
			continue
		}
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
			t.Fatalf("request %d: got %d Retry-After=%q, want 429 with Retry-After 60", i, w.Code, w.Header().Get("Retry-After"))
		}
		return
	}
	t.Fatal("1000 unauthenticated GETs from one IP never hit the limiter")
}

// TestHandleDashboard_TrustedProxy_ValidXFF_FirstServesLogin confirms the
// negative for /dashboard: a parseable XFF lets the login page render on the
// first request.
func TestHandleDashboard_TrustedProxy_ValidXFF_FirstServesLogin(t *testing.T) {
	srv := newTestServerTrustedProxy(&mockPlatform{}, "secret")

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	w := httptest.NewRecorder()
	srv.handleDashboard(w, req)

	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("unauth /dashboard with trustedProxy=true and valid XFF returned 429 on first request — gate is over-rejecting")
	}
}

// TestHandleUpgrade_TrustedProxy_MissingXFF_FailsClosed pins R20260614-SEC-10
// (#2120) for the WS upgrade path (wshub_upgrade.go). The WSUpgradeAllow
// limiter previously shared unknownIPKey for every XFF-less upgrade in
// trusted-proxy mode; the gate must now reject up front.
func TestHandleUpgrade_TrustedProxy_MissingXFF_FailsClosed(t *testing.T) {
	srv := newTestServerTrustedProxy(&mockPlatform{}, "secret")

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.RemoteAddr = "10.0.0.1:1234" // no XFF
	w := httptest.NewRecorder()
	srv.hub.HandleUpgrade(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("WS upgrade with trustedProxy=true and missing XFF returned %d, want 429 (fail-closed); the unknownIPKey shared-bucket regression is back", w.Code)
	}
}

// TestHandleUpgrade_TrustedProxy_MissingXFF_SetsRetryAfter pins
// R202606-CORR-002: the fail-closed 429 branch for unresolvable IPs in
// wshub_upgrade.go must carry Retry-After: 60, consistent with the sibling
// 429 paths in routes.go and health.go.
func TestHandleUpgrade_TrustedProxy_MissingXFF_SetsRetryAfter(t *testing.T) {
	srv := newTestServerTrustedProxy(&mockPlatform{}, "secret")

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.RemoteAddr = "10.0.0.1:1234" // no XFF — triggers fail-closed branch
	w := httptest.NewRecorder()
	srv.hub.HandleUpgrade(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want \"60\" (R202606-CORR-002: missing header regression)", got)
	}
}

// TestHandleUpgrade_LimiterDeny_SetsRetryAfter pins R202606-CORR-002: the
// per-IP limiterFn denial branch in wshub_upgrade.go must also carry
// Retry-After: 60, consistent with the sibling 429 paths.
func TestHandleUpgrade_LimiterDeny_SetsRetryAfter(t *testing.T) {
	// Use a non-trustedProxy server so RemoteAddr always resolves, then inject
	// an always-deny upgrade limiter to exercise the second 429 branch.
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Backend:        "claude",
		DashboardToken: "secret",
		TrustedProxy:   false,
	})
	srv.hub.admit.upgradeLimiter = func(ip string) bool { return false }

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.RemoteAddr = "203.0.113.5:9000"
	w := httptest.NewRecorder()
	srv.hub.HandleUpgrade(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 from limiter deny, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want \"60\" (R202606-CORR-002: missing header regression on limiterFn deny branch)", got)
	}
}
