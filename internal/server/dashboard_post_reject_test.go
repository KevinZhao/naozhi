package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardPOST_RejectedWithoutBodyEcho locks R243-SEC-15 (#800):
// the login form's HTML5 fallback used to action="/dashboard", so a JS-disabled
// browser would POST `token=…` to the dashboard handler in plaintext. The form
// is now action="javascript:void(0)" and a POST /dashboard route returns 405
// without ever reading or echoing the request body — defence-in-depth so a
// stale cached login page or a hand-crafted client cannot leak the token into
// our address space (and so a future regression that adds body-reading to the
// 405 path will be caught by the body-not-reflected assertion below).
func TestDashboardPOST_RejectedWithoutBodyEcho(t *testing.T) {
	s := newTestServer(&mockPlatform{})

	probe := "supersecret-token-do-not-leak"
	body := strings.NewReader("token=" + probe)
	req := httptest.NewRequest(http.MethodPost, "/dashboard", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow header = %q, want %q", got, "GET")
	}
	if strings.Contains(w.Body.String(), probe) {
		t.Errorf("response body echoed token bytes — must not reflect any request body")
	}
}

// TestDashboardLoginForm_NoPostFallback ensures the login HTML never reverts
// to action="/dashboard". The form action must either be javascript:void(0)
// (current) or a same-origin no-token endpoint — never a route that would
// receive token bytes in the body when JS is disabled.
func TestDashboardLoginForm_NoPostFallback(t *testing.T) {
	if strings.Contains(loginPageHTML, `action="/dashboard"`) {
		t.Fatal(`loginPageHTML must not action="/dashboard" (R243-SEC-15 / #800)`)
	}
	if !strings.Contains(loginPageHTML, `action="javascript:void(0)"`) {
		t.Errorf(`loginPageHTML form action regressed: expected "javascript:void(0)" so the browser cannot submit the token via HTML fallback`)
	}
	if !strings.Contains(loginPageHTML, "<noscript>") {
		t.Errorf("loginPageHTML missing <noscript> hint that JS is required")
	}
}
