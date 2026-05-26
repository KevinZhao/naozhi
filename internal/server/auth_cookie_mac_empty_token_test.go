package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCookieMAC_EmptyTokenReturnsEmptyString pins R245-SEC-9 (#832).
//
// When dashboardToken is unset, isAuthenticated short-circuits to true at
// the top of the function — so the cookie path is never read in production.
// Regardless, cookieMAC must NOT return a deterministic HMAC over the empty
// string: a future regression that reorders isAuthenticated's branches (or
// adds a new caller that doesn't replicate the early-return) would otherwise
// accept a forged "" cookie value because subtle.ConstantTimeCompare("", "")
// returns 1.
//
// The contract is: token=="" → cookieMAC()=="". Together with the
// `expected == ""` defence in isAuthenticated, this gives two independent
// guards on the no-token path; this test pins the first one so the second
// one alone cannot silently regress.
func TestCookieMAC_EmptyTokenReturnsEmptyString(t *testing.T) {
	t.Parallel()

	a := &AuthHandlers{
		dashboardToken: "",
		cookieSecret:   []byte("secret-bytes-here"),
		cookieGen:      "gen1",
	}

	if got := a.cookieMAC(); got != "" {
		t.Errorf("cookieMAC() with empty token = %q, want empty string\n"+
			"regression: HMAC over the empty input is deterministic, which means\n"+
			"any caller comparing against cookieMAC() would accept a forged \"\" cookie.\n"+
			"The early-return at the top of cookieMAC must not be removed.", got)
	}

	// Defence-in-depth: prove the isAuthenticated cookie path also rejects
	// a "" cookie even if a future caller sneaks past the dashboardToken
	// short-circuit. We exercise the explicit `expected == "" → false`
	// branch by sending a request with token="" but injecting a cookie
	// whose value is empty (browsers never send these, so this is a
	// scripted-attacker shape).
	r := httptest.NewRequest(http.MethodGet, "/some-protected", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: ""})
	// isAuthenticated short-circuits on dashboardToken=="" with `return true`,
	// which is the documented behaviour. The defence-in-depth check inside
	// the cookie branch only triggers when dashboardToken != "" but
	// cookieMAC() returns "" anyway. We can't easily reach that branch from
	// outside without forcing a stub, so the cookieMAC() == "" assertion
	// above is the load-bearing pin; the rest is documentation.
	if got := a.isAuthenticated(r); !got {
		t.Errorf("isAuthenticated with empty dashboardToken = %v, want true (top-of-fn short-circuit)", got)
	}
}

// TestCookieMAC_NonEmptyTokenReturnsHex is a sibling pin: when the token is
// configured, cookieMAC must produce a non-empty hex digest. Without this
// the empty-token assertion above could be silenced by a `return ""` blanket
// regression that breaks every authenticated request — the failure mode
// would only surface in an integration test, not the unit suite.
func TestCookieMAC_NonEmptyTokenReturnsHex(t *testing.T) {
	t.Parallel()

	a := &AuthHandlers{
		dashboardToken: "configured-token",
		cookieSecret:   []byte("secret-bytes-here"),
		cookieGen:      "gen1",
	}

	got := a.cookieMAC()
	if got == "" {
		t.Fatal("cookieMAC() with configured token returned \"\"; auth cookies would never validate.")
	}
	// SHA-256 hex is 64 chars regardless of input. Pin the length so a
	// future migration to a different MAC primitive is forced through this
	// test (and the surrounding contract review).
	if len(got) != 64 {
		t.Errorf("cookieMAC() length = %d, want 64 (sha256 hex). primitive change requires a contract review.", len(got))
	}
}
