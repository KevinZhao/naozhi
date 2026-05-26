package server

import (
	"net/http/httptest"
	"testing"

	"golang.org/x/time/rate"
)

// TestIPLimiter_TrustedProxy_RejectsMissingXFF pins R244-SEC-P3-3 (#897):
// when trustedProxy=true and the request carries no parseable
// X-Forwarded-For, AllowRequest MUST return false WITHOUT consulting the
// shared unknownIPKey bucket. The shared bucket is a DoS amplifier — a
// single attacker hitting the origin directly (bypassing the trusted proxy)
// would otherwise burn the bucket and 429 every other XFF-less probe.
//
// Regression shape: a future refactor that drops the
// requestHasResolvableClientIP gate and lets ip="" fall through to
// unknownIPKey would silently revert this defence; the assertion below
// catches the moment the gate stops failing-closed.
func TestIPLimiter_TrustedProxy_RejectsMissingXFF(t *testing.T) {
	// Generous budget so an unintended unknownIPKey hit would NOT fail
	// the limiter — the gate must be the rejection source, not exhaustion.
	l := newIPLimiterWithProxy(rate.Limit(1000), 1000, true)

	// 100 consecutive requests with no XFF must all be rejected. If the
	// gate were missing, the first request would be allowed (unknownIPKey
	// bucket has full budget) and the test would surface the regression.
	for i := 0; i < 100; i++ {
		r := httptest.NewRequest("GET", "/api/anything", nil)
		// RemoteAddr present (server.go always sets it) but no XFF — the
		// trustedProxy mode says this is either misconfig or origin-direct
		// abuse, both should be denied.
		r.RemoteAddr = "10.0.0.1:1234"
		if l.AllowRequest(r) {
			t.Fatalf("AllowRequest #%d returned true with trustedProxy=true and missing XFF; the unknownIPKey shared bucket regression is back", i)
		}
	}
}

// TestIPLimiter_TrustedProxy_AcceptsValidXFF confirms the negative case
// above is not the limiter just always returning false — a parseable XFF
// resumes per-IP rate limiting via clientIP.
func TestIPLimiter_TrustedProxy_AcceptsValidXFF(t *testing.T) {
	l := newIPLimiterWithProxy(rate.Limit(1000), 1000, true)
	r := httptest.NewRequest("GET", "/api/anything", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	if !l.AllowRequest(r) {
		t.Fatalf("AllowRequest returned false with trustedProxy=true and valid XFF; per-IP path is broken")
	}
}

// TestIPLimiter_NoTrustedProxy_AcceptsMissingXFF ensures the hard-gate is
// scoped to trustedProxy=true. Without trustedProxy, the limiter falls
// through to RemoteAddr, which net/http always sets.
func TestIPLimiter_NoTrustedProxy_AcceptsMissingXFF(t *testing.T) {
	l := newIPLimiterWithProxy(rate.Limit(1000), 1000, false)
	r := httptest.NewRequest("GET", "/api/anything", nil)
	r.RemoteAddr = "203.0.113.5:9999"
	if !l.AllowRequest(r) {
		t.Fatalf("AllowRequest returned false with trustedProxy=false; RemoteAddr fallback is broken")
	}
}

// TestRequestHasResolvableClientIP_TrustedProxy_XFFShapes validates the
// XFF parse rules used by the gate: tail of comma list, surrounding
// whitespace, and ParseIP rejection of garbage.
func TestRequestHasResolvableClientIP_TrustedProxy_XFFShapes(t *testing.T) {
	cases := []struct {
		name string
		xff  string
		want bool
	}{
		{"empty", "", false},
		{"single_valid", "203.0.113.5", true},
		{"chain_valid_tail", "10.0.0.1, 203.0.113.5", true},
		{"chain_with_whitespace", "10.0.0.1,  203.0.113.5", true},
		{"chain_garbage_tail", "10.0.0.1, not-an-ip", false},
		{"only_garbage", "not-an-ip", false},
		{"trailing_comma", "10.0.0.1,", false},
		{"ipv6", "2001:db8::1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = "10.0.0.1:1234"
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			got := requestHasResolvableClientIP(r, true)
			if got != tc.want {
				t.Errorf("requestHasResolvableClientIP(xff=%q, trustedProxy=true) = %v, want %v", tc.xff, got, tc.want)
			}
		})
	}
}
