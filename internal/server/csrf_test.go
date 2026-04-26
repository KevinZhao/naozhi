package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsSafeMethod locks which HTTP methods bypass the CSRF Origin gate —
// per RFC 7231 §4.2.1 GET/HEAD/OPTIONS are "safe" and so skip same-origin
// checks so bookmarks, CORS preflights, and HEAD probes keep working.
func TestIsSafeMethod(t *testing.T) {
	safe := []string{http.MethodGet, http.MethodHead, http.MethodOptions}
	unsafe := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, m := range safe {
		if !isSafeMethod(m) {
			t.Errorf("isSafeMethod(%q) = false, want true", m)
		}
	}
	for _, m := range unsafe {
		if isSafeMethod(m) {
			t.Errorf("isSafeMethod(%q) = true, want false", m)
		}
	}
}

// TestRequestHost covers the X-Forwarded-Host honoring path behind the
// trustedProxy flag and the r.Host fallback otherwise.
func TestRequestHost(t *testing.T) {
	cases := []struct {
		name         string
		host         string
		fwd          string
		trustedProxy bool
		want         string
	}{
		{"no_proxy_plain_host", "naozhi.example:8180", "", false, "naozhi.example:8180"},
		{"no_proxy_ignores_fwd", "naozhi.example", "evil.example", false, "naozhi.example"},
		{"trusted_proxy_uses_fwd", "internal:8180", "naozhi.example", true, "naozhi.example"},
		{"trusted_proxy_fallback_when_missing", "naozhi.example", "", true, "naozhi.example"},
		{"trusted_proxy_multi_value_picks_first", "internal", "naozhi.example, cache.example", true, "naozhi.example"},
		{"trusted_proxy_trims_whitespace", "internal", "  naozhi.example  , cache", true, "naozhi.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/x", nil)
			r.Host = tc.host
			if tc.fwd != "" {
				r.Header.Set("X-Forwarded-Host", tc.fwd)
			}
			if got := requestHost(r, tc.trustedProxy); got != tc.want {
				t.Errorf("requestHost = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSameOriginOK exercises the full decision matrix of the CSRF Origin
// gate — same-origin, cross-origin, missing-Origin (Referer fallback),
// "null" opaque origins, and trustedProxy X-Forwarded-Host pass-through.
//
// This is a regression test for R26-SEC1 / R31-SEC1 / R58-SEC-001 /
// R60-SEC-001: SameSite=Strict cookies do not block a sibling-subdomain
// attacker (evil.naozhi-host.example → naozhi-host.example), so we close
// that gap at the HTTP layer and lock the behavior here.
func TestSameOriginOK(t *testing.T) {
	const host = "naozhi.example:8180"
	cases := []struct {
		name         string
		origin       string
		referer      string
		host         string
		fwdHost      string
		trustedProxy bool
		want         bool
	}{
		{"same_origin_match", "http://" + host, "", host, "", false, true},
		{"same_origin_https", "https://" + host, "", host, "", false, true},
		{"cross_origin_sibling_subdomain", "http://evil." + host, "", host, "", false, false},
		{"cross_origin_different_port", "http://naozhi.example:9999", "", host, "", false, false},
		{"opaque_null_origin_rejected", "null", "", host, "", false, false},
		{"missing_origin_and_referer_allowed", "", "", host, "", false, true},
		{"referer_fallback_same_host", "", "http://" + host + "/dashboard", host, "", false, true},
		{"referer_fallback_cross_host", "", "http://evil.example/evil.html", host, "", false, false},
		{"referer_malformed_rejected", "", "::not-a-url::", host, "", false, false},
		{"origin_malformed_rejected", "::not-a-url::", "", host, "", false, false},
		{"empty_host_refuses", "http://foo", "", "", "", false, false},
		{"trusted_proxy_forwarded_host_match", "https://naozhi.example", "", "internal:8180", "naozhi.example", true, true},
		{"trusted_proxy_forwarded_host_mismatch", "https://evil.example", "", "internal:8180", "naozhi.example", true, false},
		{"untrusted_proxy_ignores_forwarded_host", "https://naozhi.example", "", "internal:8180", "naozhi.example", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/x", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			if tc.fwdHost != "" {
				r.Header.Set("X-Forwarded-Host", tc.fwdHost)
			}
			if got := sameOriginOK(r, tc.trustedProxy); got != tc.want {
				t.Errorf("sameOriginOK = %v, want %v (origin=%q referer=%q host=%q fwd=%q proxy=%v)",
					got, tc.want, tc.origin, tc.referer, tc.host, tc.fwdHost, tc.trustedProxy)
			}
		})
	}
}

// TestRequireAuth_CSRFGate verifies that requireAuth rejects mutating
// cross-origin requests even when the session cookie is valid — closing
// the same-registrable-domain CSRF gap that SameSite=Strict does not cover.
// Safe methods (GET) must still pass so bookmarks and external links work.
func TestRequireAuth_CSRFGate(t *testing.T) {
	a := &AuthHandlers{dashboardToken: ""} // empty token => isAuthenticated=true, so we isolate the Origin gate
	handler := a.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name   string
		method string
		origin string
		want   int
	}{
		{"post_same_origin", http.MethodPost, "http://naozhi.example", http.StatusOK},
		{"post_cross_origin", http.MethodPost, "http://evil.example", http.StatusForbidden},
		{"post_null_origin", http.MethodPost, "null", http.StatusForbidden},
		{"get_cross_origin_allowed", http.MethodGet, "http://evil.example", http.StatusOK},
		{"get_no_origin", http.MethodGet, "", http.StatusOK},
		{"post_no_origin_and_no_referer", http.MethodPost, "", http.StatusOK}, // non-browser client
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://naozhi.example/api/x", nil)
			r.Host = "naozhi.example"
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%q)", w.Code, tc.want, strings.TrimSpace(w.Body.String()))
			}
		})
	}
}

// TestHandleLogin_CSRFGate verifies the same-origin gate inside
// handleLogin (which sits OUTSIDE requireAuth because it is the endpoint
// that grants auth in the first place). A cross-origin login POST must be
// refused before the token is even compared, so a misconfigured proxy
// cannot leak the dashboard token via a preflight or reflected error body.
func TestHandleLogin_CSRFGate(t *testing.T) {
	a := &AuthHandlers{
		dashboardToken: "secret",
		cookieSecret:   []byte("cookie"),
		loginLimiter:   newLoginLimiter(),
	}
	r := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/auth/login",
		strings.NewReader(`{"token":"secret"}`))
	r.Host = "naozhi.example"
	r.Header.Set("Origin", "http://evil.example")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleLogin(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login: status = %d, want %d (body=%q)",
			w.Code, http.StatusForbidden, strings.TrimSpace(w.Body.String()))
	}
}
