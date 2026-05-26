package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/ratelimit"
)

// TestHandleHealth_UnauthRateLimited locks the R246-SEC-11 (#819)
// contract: anonymous /health probes must be throttled by the same
// per-IP unauthDashLimiter that gates GET /dashboard. Without this
// gate, the unauthenticated branch leaks status + uptime to anyone
// on the network at unbounded request rate, which:
//
//  1. Lets an external scanner fingerprint the deployment by
//     correlating uptime drift across two requests (uptime
//     resolution is 1s — a sample at T and T+5s reveals "process
//     restarted in the last 5s" with no token needed).
//  2. Lets a botnet sustain a high-volume probe loop without
//     triggering any back-pressure, since /health does not flow
//     through the dashboard's middleware.
//
// We construct an unauthDashLimiter with a tight 1/sec / burst=1
// budget so the test does not need to fire 60 requests; the
// production limiter is 60/min / burst=20 so the same code path
// applies, just at a different rate.
func TestHandleHealth_UnauthRateLimited(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	// Tight bucket so the test only needs 2 requests to trip the limiter.
	srv.auth.unauthDashLimiter = ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(time.Hour), // effectively no refill within the test
		Burst:   1,
		MaxKeys: 16,
		TTL:     time.Minute,
	})

	mkReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.RemoteAddr = "10.0.0.42:1234"
		return r
	}

	// First probe consumes the burst — must succeed (200 OK).
	w1 := httptest.NewRecorder()
	srv.healthH.handleHealth(w1, mkReq())
	if w1.Code != http.StatusOK {
		t.Fatalf("first unauth probe: status = %d, want 200", w1.Code)
	}

	// Second probe from the same IP exhausts the bucket — must 429.
	w2 := httptest.NewRecorder()
	srv.healthH.handleHealth(w2, mkReq())
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second unauth probe from same IP: status = %d, want 429 (rate limit must trip)", w2.Code)
	}

	// Probe from a different IP must still succeed (per-IP buckets, not global).
	r3 := httptest.NewRequest(http.MethodGet, "/health", nil)
	r3.RemoteAddr = "10.0.0.43:1234"
	w3 := httptest.NewRecorder()
	srv.healthH.handleHealth(w3, r3)
	if w3.Code != http.StatusOK {
		t.Errorf("probe from different IP: status = %d, want 200 (limiter must be per-IP, not global)", w3.Code)
	}
}

// TestHandleHealth_AuthBypassesRateLimit verifies the gate is restricted
// to the unauthenticated branch. Authenticated probes (operator dashboard
// at 1Hz × N tabs, /api/sessions polling) must NEVER hit 429 from the
// unauthDashLimiter — they have their own ambient throttle (the dashboard
// poll cadence) and isAuthenticated short-circuits the limiter check.
//
// Without this guard a stolen-token attacker would face the same anonymous
// rate budget, but the legitimate dashboard's reload+poll burst on
// multi-tab use would also trip 429 and break the operator UI. Lock the
// behavioural contract so a future limiter refactor can't accidentally
// degrade authenticated flows.
func TestHandleHealth_AuthBypassesRateLimit(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	// Tight bucket — would block any unauth caller after the first request.
	srv.auth.unauthDashLimiter = ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(time.Hour),
		Burst:   1,
		MaxKeys: 16,
		TTL:     time.Minute,
	})

	// Burn the burst with one anonymous probe.
	rUnauth := httptest.NewRequest(http.MethodGet, "/health", nil)
	rUnauth.RemoteAddr = "10.0.0.99:1234"
	srv.healthH.handleHealth(httptest.NewRecorder(), rUnauth)

	// Authenticated probe from the SAME IP must still succeed.
	rAuth := httptest.NewRequest(http.MethodGet, "/health", nil)
	rAuth.RemoteAddr = "10.0.0.99:1234"
	rAuth.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, rAuth)
	if w.Code != http.StatusOK {
		t.Errorf("authenticated /health from rate-limited IP: status = %d, want 200 (auth must bypass unauth limiter)", w.Code)
	}
}
