package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleLogout_RevokesOutstandingCookie pins S9 (#389): /api/logout must
// not merely clear the browser-side cookie — it must invalidate the underlying
// MAC so a stolen cookie (still within the 24h MaxAge) can no longer replay.
//
// Pre-fix, HandleLogout only emitted a MaxAge=-1 Set-Cookie; an attacker who
// had already captured the cookie value kept authenticating because cookieGen
// was never bumped. The fix calls RotateCookieGen so the issued MAC fails the
// constant-time compare on every subsequent request.
func TestHandleLogout_RevokesOutstandingCookie(t *testing.T) {
	t.Parallel()
	a := &Handlers{
		DashboardToken: "logout-revoke-token",
		cookieSecret:   []byte("logout-revoke-secret"),
	}

	stolen := a.CookieMAC()
	if stolen == "" {
		t.Fatal("CookieMAC unexpectedly empty for non-empty token")
	}

	// Sanity: the captured cookie authenticates before logout.
	preReq := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/whatever", nil)
	preReq.AddCookie(&http.Cookie{Name: AuthCookieName, Value: stolen})
	if !a.IsAuthenticated(preReq) {
		t.Fatal("pre-logout cookie did not authenticate — test setup broken")
	}

	// Invoke logout.
	logoutReq := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/logout", nil)
	rec := httptest.NewRecorder()
	a.HandleLogout(rec, logoutReq)

	// The browser is told to drop the cookie...
	if mc := rec.Result().Cookies(); len(mc) == 0 || mc[0].MaxAge != -1 {
		t.Fatalf("logout must emit a clearing Set-Cookie (MaxAge=-1), got %+v", mc)
	}

	// ...AND the captured cookie value must now be rejected server-side: a
	// stolen-cookie replay must fail after logout.
	replayReq := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/whatever", nil)
	replayReq.AddCookie(&http.Cookie{Name: AuthCookieName, Value: stolen})
	if a.IsAuthenticated(replayReq) {
		t.Error("after logout, the previously-issued cookie still authenticated — " +
			"logout did not revoke the underlying MAC (#389)")
	}
}
