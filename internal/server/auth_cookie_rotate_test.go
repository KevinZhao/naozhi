package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRotateCookieGen_InvalidatesOutstandingMAC pins R245-SEC-2 (#826):
// calling RotateCookieGen MUST change cookieMAC so a previously-issued
// cookie value no longer authenticates. Pre-fix, cookieGen was seeded
// once at construction with no public bump path — so a hot-reload of
// dashboardToken (or any future secret rotation event) left existing
// cookies valid for the full 24h MaxAge.
//
// Contract: cookieMAC()_before != cookieMAC()_after, AND a request
// carrying the before-value cookie must be rejected by isAuthenticated
// after the rotation. Both layers asserted so a future refactor that
// removes the seq mix from cookieMAC (but keeps RotateCookieGen) is
// caught by the auth-flow assertion, and a refactor that drops
// RotateCookieGen entirely is caught by the MAC-equality assertion.
func TestRotateCookieGen_InvalidatesOutstandingMAC(t *testing.T) {
	t.Parallel()
	a := &AuthHandlers{
		dashboardToken: "rotate-test-token",
		cookieSecret:   []byte("rotate-test-secret"),
	}

	before := a.cookieMAC()
	if before == "" {
		t.Fatal("cookieMAC unexpectedly empty for non-empty token")
	}

	// Pre-rotate: a request carrying the before-value cookie must auth.
	req := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/whatever", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: before})
	if !a.isAuthenticated(req) {
		t.Fatal("pre-rotate cookie did not authenticate — test setup broken")
	}

	a.RotateCookieGen()

	after := a.cookieMAC()
	if after == before {
		t.Errorf("cookieMAC unchanged after RotateCookieGen — seq not mixed into HMAC?\n"+
			"before=%q after=%q", before, after)
	}

	// Post-rotate: the same before-value cookie must NOT auth (the
	// browser would still be carrying it; rotation's whole point is
	// that those cookies are no longer good).
	rejReq := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/whatever", nil)
	rejReq.AddCookie(&http.Cookie{Name: authCookieName, Value: before})
	if a.isAuthenticated(rejReq) {
		t.Errorf("after RotateCookieGen, the pre-rotate cookie still authenticated — "+
			"hot-rotate is broken (#826).\nbefore=%q after=%q", before, after)
	}

	// Sanity: the post-rotate cookie still authenticates so legitimate
	// re-login isn't broken.
	okReq := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/whatever", nil)
	okReq.AddCookie(&http.Cookie{Name: authCookieName, Value: after})
	if !a.isAuthenticated(okReq) {
		t.Error("post-rotate cookie failed to authenticate — RotateCookieGen broke the auth flow")
	}
}

// TestRotateCookieGen_SuccessiveBumpsKeepRotating pins that RotateCookieGen
// is monotonic — successive calls keep producing distinct MACs. Without
// this property, a single rotation could land on the same MAC by accident
// (e.g. a future "rotate to N" semantic that picks N from a small
// numeric space).
func TestRotateCookieGen_SuccessiveBumpsKeepRotating(t *testing.T) {
	t.Parallel()
	a := &AuthHandlers{
		dashboardToken: "monotonic-test",
		cookieSecret:   []byte("monotonic-secret"),
	}

	macs := make(map[string]struct{})
	macs[a.cookieMAC()] = struct{}{}
	for i := 0; i < 5; i++ {
		a.RotateCookieGen()
		m := a.cookieMAC()
		if _, dup := macs[m]; dup {
			t.Fatalf("RotateCookieGen iter %d collided with a prior MAC %q", i, m)
		}
		macs[m] = struct{}{}
	}
	if len(macs) != 6 {
		t.Errorf("expected 6 distinct MACs (initial + 5 bumps), got %d", len(macs))
	}
}
