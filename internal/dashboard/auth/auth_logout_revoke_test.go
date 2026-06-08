package auth

import (
	"net/http"
	"net/http/httptest"
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

	// The caller's browser is told to drop the cookie.
	if mc := rec.Result().Cookies(); len(mc) == 0 || mc[0].MaxAge != -1 {
		t.Fatalf("logout must emit a clearing Set-Cookie (MaxAge=-1), got %+v", mc)
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
