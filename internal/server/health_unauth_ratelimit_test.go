package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleHealth_Unauthenticated_RateLimited pins R246-SEC-11 (#819):
// the unauthenticated /health branch must be gated by the per-IP
// unauthDashLimiter so an attacker scanning across time cannot enumerate
// the deploy's uptime to fingerprint restart cadence. Authenticated
// callers bypass the gate (the auth check happens first), so a legitimate
// dashboard polling at 1 Hz is unaffected.
//
// We exercise the bucket by hammering the same source IP past the
// unauthDashLimiter burst (20 — see server.New / newWSUpgradeLimiter)
// and asserting that the handler eventually returns 429. The exact
// burst is wired by server.New so we drive enough requests (200) to
// make the test independent of any future bucket-size tweak as long as
// the limiter rate stays below 200/s — which it must, otherwise the
// gate would be useless.
func TestHandleHealth_Unauthenticated_RateLimited(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")

	saw429 := false
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = "10.42.42.42:33333"
		w := httptest.NewRecorder()
		srv.healthH.handleHealth(w, req)
		if w.Code == http.StatusTooManyRequests {
			saw429 = true
			if got := w.Header().Get("Retry-After"); got != "60" {
				t.Errorf("Retry-After = %q, want 60", got)
			}
			break
		}
	}
	if !saw429 {
		t.Errorf("expected 429 within 200 unauth /health probes from a single IP — limiter bypassed?")
	}
}

// TestHandleHealth_Authenticated_BypassesUnauthLimiter pins the negative:
// once a request carries a valid Bearer token, the unauth limiter must NOT
// fire. A legitimate dashboard polls at 1 Hz; if auth'd requests counted
// against the unauth bucket the dashboard would 429 itself within seconds
// of opening multiple tabs.
func TestHandleHealth_Authenticated_BypassesUnauthLimiter(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")

	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = "10.42.42.43:33333"
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		srv.healthH.handleHealth(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("authenticated /health hit 429 at probe %d — unauth limiter is firing on auth'd path", i)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("authenticated /health probe %d returned %d, want 200", i, w.Code)
		}
	}
}
