package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// findAuthCookie returns the nz_auth cookie from a response, or nil.
func findAuthCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == AuthCookieName {
			return c
		}
	}
	return nil
}

// TestRequireAuth_SlidingRenewal_RefreshesCookieOnAuthedRequest pins the
// fix for the "auth prompt fires every hour even on an active dashboard"
// report: RequireAuth must re-issue the nz_auth cookie with a fresh MaxAge
// on every request that already carried a valid cookie, so an actively-used
// tab never trips the 1h idle expiry mid-session.
//
// Contract: a GET carrying a valid cookie passes through to next AND the
// response sets a fresh nz_auth cookie with MaxAge == authCookieMaxAgeSeconds
// and the same (HMAC) value (sliding window, not a new identity).
func TestRequireAuth_SlidingRenewal_RefreshesCookieOnAuthedRequest(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "slide-token",
		cookieSecret:   []byte("slide-secret"),
		TrustedProxy:   false,
	}
	mac := a.CookieMAC()

	called := false
	h := a.RequireAuth(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/sessions", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: mac})
	rec := httptest.NewRecorder()
	h(rec, req)

	if !called {
		t.Fatal("authed request did not reach next handler")
	}
	got := findAuthCookie(rec.Result())
	if got == nil {
		t.Fatal("sliding renewal did not re-issue nz_auth cookie on authed request")
	}
	if got.MaxAge != authCookieMaxAgeSeconds {
		t.Errorf("renewed cookie MaxAge = %d, want %d", got.MaxAge, authCookieMaxAgeSeconds)
	}
	if got.Value != mac {
		t.Errorf("renewed cookie value changed: got %q want %q (must keep same HMAC so window slides without new identity)", got.Value, mac)
	}
	if !got.HttpOnly {
		t.Error("renewed cookie must stay HttpOnly")
	}
	if got.SameSite != http.SameSiteStrictMode {
		t.Errorf("renewed cookie SameSite = %v, want Strict", got.SameSite)
	}
}

// TestRequireAuth_NoRenewalForBearerToken pins that a request authenticated
// via the Authorization: Bearer header (not a cookie) must NOT be handed a
// session cookie. Bearer callers (CLI/scripts) never asked for cookie state;
// silently issuing one would be surprising and could leak the MAC into
// clients that don't manage it. Sliding renewal is scoped to browser
// cookie sessions only.
func TestRequireAuth_NoRenewalForBearerToken(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "bearer-token",
		cookieSecret:   []byte("bearer-secret"),
	}
	called := false
	h := a.RequireAuth(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer bearer-token")
	rec := httptest.NewRecorder()
	h(rec, req)

	if !called {
		t.Fatal("bearer-authed request did not reach next handler")
	}
	if got := findAuthCookie(rec.Result()); got != nil {
		t.Errorf("Bearer-authenticated request must not be issued a session cookie, got %+v", got)
	}
}

// TestRequireAuth_NoRenewalWhenUnauthenticated pins that a rejected request
// (no/invalid cookie) is not handed a fresh cookie — renewal must only
// extend an existing valid session, never bootstrap one.
func TestRequireAuth_NoRenewalWhenUnauthenticated(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "reject-token",
		cookieSecret:   []byte("reject-secret"),
	}
	called := false
	h := a.RequireAuth(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/sessions", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: "forged-value"})
	rec := httptest.NewRecorder()
	h(rec, req)

	if called {
		t.Fatal("unauthenticated request reached next handler — RequireAuth gate broken")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if got := findAuthCookie(rec.Result()); got != nil {
		t.Errorf("rejected request must not be issued a cookie, got %+v", got)
	}
}

// TestRequireAuth_NoRenewalInNoTokenMode pins that a deployment with no
// dashboard token configured (open mode) never issues a session cookie,
// even though every request is "authenticated". cookieRequestAuthenticated
// must short-circuit to false on empty token so the cookie path stays inert
// end-to-end (mirrors the CookieMAC()=="" no-token contract).
func TestRequireAuth_NoRenewalInNoTokenMode(t *testing.T) {
	t.Parallel()
	a := &Handlers{DashboardToken: "", cookieSecret: []byte("any")}
	called := false
	h := a.RequireAuth(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/sessions", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if !called {
		t.Fatal("no-token deployment must pass requests through")
	}
	if got := findAuthCookie(rec.Result()); got != nil {
		t.Errorf("no-token deployment must not issue a session cookie, got %+v", got)
	}
}
