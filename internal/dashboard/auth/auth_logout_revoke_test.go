package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleLogout_DoesNotRevokeConcurrentSessions pins R20260607-SEC-7
// (#1913): /api/logout must NOT bump the global cookie generation.
//
// Every authenticated browser is issued the same CookieMAC value (the MAC has
// no per-session entropy), so a global RotateCookieGen on logout invalidated
// *every* outstanding cookie. Since /api/logout is only RequireAuth-gated, any
// holder of a valid cookie — including a stolen one — could evict every other
// concurrent operator (denial-of-authentication). After the fix, logout clears
// only the caller's browser cookie; a cookie held by another concurrent
// session must still authenticate.
func TestHandleLogout_DoesNotRevokeConcurrentSessions(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "logout-token",
		cookieSecret:   []byte("logout-secret"),
	}

	// A second concurrent operator's cookie (same MAC under today's scheme).
	otherSession := a.CookieMAC()
	if otherSession == "" {
		t.Fatal("CookieMAC unexpectedly empty for non-empty token")
	}

	// Sanity: the other session authenticates before any logout.
	preReq := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/whatever", nil)
	preReq.AddCookie(&http.Cookie{Name: AuthCookieName, Value: otherSession})
	if !a.IsAuthenticated(preReq) {
		t.Fatal("pre-logout cookie did not authenticate — test setup broken")
	}

	// One operator logs out.
	logoutReq := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/logout", nil)
	rec := httptest.NewRecorder()
	a.HandleLogout(rec, logoutReq)

	// The caller's browser is told to drop the auth cookie. Look it up by
	// name rather than index — logout now also clears nz_anon (#2157), so the
	// auth cookie is no longer guaranteed to be at position 0.
	var authClear *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == AuthCookieName {
			authClear = c
			break
		}
	}
	if authClear == nil || authClear.MaxAge != -1 {
		t.Fatalf("logout must emit a clearing %s Set-Cookie (MaxAge=-1), got %+v",
			AuthCookieName, rec.Result().Cookies())
	}

	// The other concurrent session's cookie MUST still authenticate — logout
	// of one user may not evict everyone else.
	otherReq := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/whatever", nil)
	otherReq.AddCookie(&http.Cookie{Name: AuthCookieName, Value: otherSession})
	if !a.IsAuthenticated(otherReq) {
		t.Error("after one user's logout, a concurrent session's cookie was rejected — " +
			"logout performed a global revocation (#1913)")
	}
}

// TestHandleLogout_DoesNotBumpCookieGenSeq guards the regression at the
// mechanism level: HandleLogout must leave cookieGenSeq untouched (the global
// bump is reserved for secret-rotation events), while RotateCookieGen — the
// dedicated rotation hook — still increments it.
func TestHandleLogout_DoesNotBumpCookieGenSeq(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "seq-token",
		cookieSecret:   []byte("seq-secret"),
	}

	before := a.cookieGenSeq.Load()

	logoutReq := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/logout", nil)
	a.HandleLogout(httptest.NewRecorder(), logoutReq)

	if got := a.cookieGenSeq.Load(); got != before {
		t.Errorf("HandleLogout bumped cookieGenSeq (%d -> %d); the global bump "+
			"invalidates all concurrent sessions (#1913)", before, got)
	}

	// The rotation hook must still work for legitimate secret-rotation events.
	a.RotateCookieGen()
	if got := a.cookieGenSeq.Load(); got != before+1 {
		t.Errorf("RotateCookieGen did not bump cookieGenSeq: got %d want %d", got, before+1)
	}
}

// TestHandleLogout_ClearsAnonCookie pins #2157: logout must emit clearing
// Set-Cookie headers for BOTH the naozhi_auth credential and the nz_anon
// per-browser owner label, so logging out fully resets browser-held naozhi
// state. Previously only naozhi_auth was cleared, leaving the nz_anon owner
// label alive for its (up to 7-day) MaxAge.
func TestHandleLogout_ClearsAnonCookie(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "anon-clear-token",
		cookieSecret:   []byte("anon-clear-secret"),
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/logout", nil)
	rec := httptest.NewRecorder()
	a.HandleLogout(rec, logoutReq)

	cleared := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		cleared[c.Name] = c
	}

	for _, name := range []string{AuthCookieName, "nz_anon"} {
		c, ok := cleared[name]
		if !ok {
			t.Fatalf("logout did not emit a clearing Set-Cookie for %q; got %+v",
				name, rec.Result().Cookies())
		}
		if c.MaxAge != -1 {
			t.Errorf("%q clearing cookie MaxAge = %d, want -1", name, c.MaxAge)
		}
		if c.Value != "" {
			t.Errorf("%q clearing cookie Value = %q, want empty", name, c.Value)
		}
	}
}

// TestHandleLogin_CookieMaxAgeBounded pins R20260613-SEC-3 (#2074): because a
// stolen cookie cannot be revoked server-side without evicting every
// concurrent operator, the only server-side bound on its replay window is the
// login cookie's MaxAge. It must stay short (<= 1h) rather than the prior 24h
// so a leaked cookie expires quickly on its own. A regression back to 86400
// would silently re-open the 24h replay window.
func TestHandleLogin_CookieMaxAgeBounded(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "login-token",
		cookieSecret:   []byte("login-secret"),
		loginLimiter:   NewLoginLimiter(),
	}

	r := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/auth/login",
		strings.NewReader(`{"token":"login-token"}`))
	r.Host = "naozhi.example"
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleLogin(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("login: status = %d, want %d (body=%q)",
			rec.Code, http.StatusOK, strings.TrimSpace(rec.Body.String()))
	}

	cookies := rec.Result().Cookies()
	var auth *http.Cookie
	for _, c := range cookies {
		if c.Name == AuthCookieName {
			auth = c
			break
		}
	}
	if auth == nil {
		t.Fatalf("login did not set the %s cookie; got %+v", AuthCookieName, cookies)
	}
	if auth.MaxAge != authCookieMaxAgeSeconds {
		t.Errorf("login cookie MaxAge = %d, want %d", auth.MaxAge, authCookieMaxAgeSeconds)
	}
	// Guard against a silent regression to the old 24h window regardless of
	// what the constant is renamed to.
	if auth.MaxAge > 3600 {
		t.Errorf("login cookie MaxAge = %d exceeds the 1h replay-window bound (#2074); "+
			"a stolen cookie is unrevocable server-side, so the lifetime must stay short",
			auth.MaxAge)
	}
}
