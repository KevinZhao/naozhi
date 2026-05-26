package server

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

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

// TestNewIPLimiterWithCap_RaisesLRUFloorAboveDefault pins R242-SEC-8 / #636:
// the cron-handler limiters use newIPLimiterWithCap with an explicit
// MaxKeys above the ratelimit package default (1000). Without the explicit
// cap, a DDoS burst of fresh attacker IPs evicts the LRU tail — by
// construction a legit rate-limited entry — and lets it back through
// un-throttled. The test populates the limiter with more than 1000 distinct
// IPs to confirm the explicit cap holds: an entry inserted at IP #1
// must still be retained after IPs #2..#2000 land, because the LRU now
// has room for 8192 keys rather than 1000.
//
// Regression shape: a future refactor that drops the cap arg (e.g. by
// re-routing buildCronHandlers through plain newIPLimiterWithProxy) would
// shrink the cap back to 1000 and IP #1 would be evicted by IP #1001 —
// AllowRequest for IP #1 would then succeed (fresh bucket) instead of
// reflecting accumulated debt. We assert IP #1's bucket survives.
func TestNewIPLimiterWithCap_RaisesLRUFloorAboveDefault(t *testing.T) {
	// Burst 1, refill never (rate.Every(1h)) — first call burns the only
	// token; subsequent calls for the same key return false. After eviction
	// a fresh bucket is installed and the first call returns true again,
	// which is exactly the regression we want to catch.
	l := newIPLimiterWithCap(rate.Every(time.Hour), 1, cronLimiterMaxKeys, cronLimiterTTL, false)

	// Burn IP #1's only token.
	r1 := httptest.NewRequest("GET", "/api/cron", nil)
	r1.RemoteAddr = "10.0.0.1:1000"
	if !l.AllowRequest(r1) {
		t.Fatalf("first call for IP #1 must succeed (fresh bucket has 1 token)")
	}
	if l.AllowRequest(r1) {
		t.Fatalf("second call for IP #1 must fail (rate.Every(1h) means no refill within test wall-time)")
	}

	// Inject 2000 distinct IPs — well above the 1000-key default that #636
	// flags as a DDoS soft floor, but well below the 8192-key cron cap.
	// If MaxKeys defaulted, IP #1 would be evicted around the 1001st insert.
	for i := 2; i <= 2001; i++ {
		r := httptest.NewRequest("GET", "/api/cron", nil)
		r.RemoteAddr = fmt.Sprintf("10.0.%d.%d:1000", i/256, i%256)
		_ = l.AllowRequest(r)
	}

	// IP #1's debt MUST still be honoured. If the cap were defaulted to
	// 1000, IP #1 would have been evicted and a fresh bucket installed,
	// which would then return true — the regression marker.
	if l.AllowRequest(r1) {
		t.Fatalf("IP #1's bucket was evicted under 2000-key load — explicit cronLimiterMaxKeys (%d) regressed to package default (1000)", cronLimiterMaxKeys)
	}
}

// TestNewIPLimiterWithProxy_DefaultMaxKeysAlignsWithAuth pins R241-SEC-14
// / #473: every newIPLimiterWithProxy bucket MUST inherit the 10000-key
// LRU cap that the auth-side limiters (loginLimiter / wsUpgradeLimiter)
// explicitly set, rather than the 1000-key ratelimit package default.
// Misalignment means a flood of fresh attacker IPs evicts older legit
// rate-limited entries from the unspecified buckets (uploadLimiter /
// sendLimiter / openLimit / filesExistsLimiter / configPutLimiter /
// transcribeLimiter / dashboard memory limiter) while the auth side
// stays correctly sized — uneven hardening across paths facing the same
// threat model.
//
// We exercise the cap by inserting 1500 distinct IPs after burning IP
// #1's bucket. With the default still at 1000 (the regression we lock
// against) IP #1 is evicted around insert 1001 and a fresh bucket lets
// it back through; with the explicit 10000-key default it is preserved.
//
// The test uses newIPLimiterWithProxy directly so a future refactor that
// removes the explicit MaxKeys/TTL pass — even silently via a code-mod
// of the helper — re-surfaces the regression.
func TestNewIPLimiterWithProxy_DefaultMaxKeysAlignsWithAuth(t *testing.T) {
	l := newIPLimiterWithProxy(rate.Every(time.Hour), 1, false)

	r1 := httptest.NewRequest("GET", "/api/anything", nil)
	r1.RemoteAddr = "10.0.0.1:1000"
	if !l.AllowRequest(r1) {
		t.Fatalf("first call for IP #1 must succeed (fresh bucket)")
	}
	if l.AllowRequest(r1) {
		t.Fatalf("second call for IP #1 must fail (no refill in test wall-time)")
	}

	// 1500 distinct IPs — overshoots the 1000-key default by 50% to
	// guarantee eviction would have happened, while staying well under
	// the 10000-key explicit cap.
	for i := 2; i <= 1501; i++ {
		r := httptest.NewRequest("GET", "/api/anything", nil)
		r.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:1000", i/65536, (i/256)%256, i%256)
		_ = l.AllowRequest(r)
	}

	if l.AllowRequest(r1) {
		t.Fatalf("R241-SEC-14 regression: IP #1 evicted under 1500-key load — newIPLimiterWithProxy default cap regressed below %d", defaultIPLimiterMaxKeys)
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
