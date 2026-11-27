package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRotateCookieGen_InvalidatesOutstandingMAC pins R245-SEC-2 (#826):
// calling RotateCookieGen MUST change CookieMAC so a previously-issued
// cookie value no longer authenticates. Pre-fix, cookieGen was seeded
// once at construction with no public bump path — so a hot-reload of
// DashboardToken (or any future secret rotation event) left existing
// cookies valid for the full 24h MaxAge.
//
// Contract: CookieMAC()_before != CookieMAC()_after, AND a request
// carrying the before-value cookie must be rejected by IsAuthenticated
// after the rotation. Both layers asserted so a future refactor that
// removes the seq mix from CookieMAC (but keeps RotateCookieGen) is
// caught by the auth-flow assertion, and a refactor that drops
// RotateCookieGen entirely is caught by the MAC-equality assertion.
func TestRotateCookieGen_InvalidatesOutstandingMAC(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "rotate-test-token",
		cookieSecret:   []byte("rotate-test-secret"),
	}

	before := a.CookieMAC()
	if before == "" {
		t.Fatal("CookieMAC unexpectedly empty for non-empty token")
	}

	// Pre-rotate: a request carrying the before-value cookie must auth.
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/whatever", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: before})
	if !a.IsAuthenticated(req) {
		t.Fatal("pre-rotate cookie did not authenticate — test setup broken")
	}

	a.RotateCookieGen()

	after := a.CookieMAC()
	if after == before {
		t.Errorf("CookieMAC unchanged after RotateCookieGen — seq not mixed into HMAC?\n"+
			"before=%q after=%q", before, after)
	}

	// Post-rotate: the same before-value cookie must NOT auth (the
	// browser would still be carrying it; rotation's whole point is
	// that those cookies are no longer good).
	rejReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/whatever", nil)
	rejReq.AddCookie(&http.Cookie{Name: AuthCookieName, Value: before})
	if a.IsAuthenticated(rejReq) {
		t.Errorf("after RotateCookieGen, the pre-rotate cookie still authenticated — "+
			"hot-rotate is broken (#826).\nbefore=%q after=%q", before, after)
	}

	// Sanity: the post-rotate cookie still authenticates so legitimate
	// re-login isn't broken.
	okReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/whatever", nil)
	okReq.AddCookie(&http.Cookie{Name: AuthCookieName, Value: after})
	if !a.IsAuthenticated(okReq) {
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
	a := &Handlers{
		DashboardToken: "monotonic-test",
		cookieSecret:   []byte("monotonic-secret"),
	}

	macs := make(map[string]struct{})
	macs[a.CookieMAC()] = struct{}{}
	for i := 0; i < 5; i++ {
		a.RotateCookieGen()
		m := a.CookieMAC()
		if _, dup := macs[m]; dup {
			t.Fatalf("RotateCookieGen iter %d collided with a prior MAC %q", i, m)
		}
		macs[m] = struct{}{}
	}
	if len(macs) != 6 {
		t.Errorf("expected 6 distinct MACs (initial + 5 bumps), got %d", len(macs))
	}
}

// TestCookieMAC_EmptyTokenReturnsEmpty pins R245-SEC-9 (#832): when
// DashboardToken is empty, CookieMAC MUST return "" rather than the
// HMAC over the empty string. The previous form computed a deterministic
// MAC over zero-length input that any caller could replay; IsAuthenticated
// already short-circuits to true on empty token so the value was unused
// today, but a future regression that re-orders the no-token short-circuit
// must not be able to accept a forged "" cookie. Pinning the contract at
// the producer keeps the no-token path explicit at the source.
func TestCookieMAC_EmptyTokenReturnsEmpty(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "",
		cookieSecret:   []byte("any-secret"),
	}
	if got := a.CookieMAC(); got != "" {
		t.Errorf("CookieMAC over empty token must return \"\", got %q", got)
	}
	// Bumping cookieGenSeq must not change the empty-token contract — a
	// future RotateCookieGen invocation on a no-token deployment must keep
	// returning "" so the cookie code path stays inert end-to-end.
	a.RotateCookieGen()
	if got := a.CookieMAC(); got != "" {
		t.Errorf("CookieMAC over empty token must stay \"\" after RotateCookieGen, got %q", got)
	}
}
