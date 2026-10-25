package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
