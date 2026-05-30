package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNew_EmptyCookieGenIsRandomised pins R241-SEC-10 (#470): when New is
// called without an explicit cookieGen, the cookie MAC must NOT be a
// deterministic function of (token, secret) alone. Pre-fix, an empty gen made
// CookieMAC reproducible across processes, so a captured cookie authenticated
// against any future instance sharing the same token + secret. The fix seeds a
// random per-construction gen, so two instances built with identical inputs
// produce different MACs.
func TestNew_EmptyCookieGenIsRandomised(t *testing.T) {
	t.Parallel()

	const (
		token  = "rand-gen-token"
		secret = "rand-gen-secret"
	)
	a := New(token, []byte(secret), "", false)
	b := New(token, []byte(secret), "", false)

	macA := a.CookieMAC()
	macB := b.CookieMAC()
	if macA == "" || macB == "" {
		t.Fatalf("CookieMAC unexpectedly empty (macA=%q macB=%q)", macA, macB)
	}
	if macA == macB {
		t.Errorf("two instances with identical (token, secret) and empty gen produced "+
			"the same MAC %q — empty-gen path is still deterministic (#470)", macA)
	}

	// A cookie issued by instance a must not authenticate against instance b
	// (different gen → different MAC). This is the cross-process replay window
	// the fix closes.
	req := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/x", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: macA})
	if b.IsAuthenticated(req) {
		t.Error("a cookie from instance a authenticated against instance b — " +
			"empty-gen instances share a deterministic MAC (#470)")
	}
	// Sanity: the cookie still authenticates against its own issuer.
	selfReq := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/x", nil)
	selfReq.AddCookie(&http.Cookie{Name: AuthCookieName, Value: macA})
	if !a.IsAuthenticated(selfReq) {
		t.Error("cookie failed to authenticate against its own issuer — randomised gen broke the happy path")
	}
}

// TestNew_ExplicitCookieGenPreserved pins that a caller-supplied gen is used
// verbatim (production seeds one in server.go); the randomisation only kicks
// in on the empty-gen fallback path.
func TestNew_ExplicitCookieGenPreserved(t *testing.T) {
	t.Parallel()

	const (
		token  = "explicit-gen-token"
		secret = "explicit-gen-secret"
		gen    = "fixed-seed-123"
	)
	a := New(token, []byte(secret), gen, false)
	b := New(token, []byte(secret), gen, false)
	if a.CookieMAC() != b.CookieMAC() {
		t.Error("identical explicit gens must yield identical MACs — caller-supplied gen was overridden")
	}
}
