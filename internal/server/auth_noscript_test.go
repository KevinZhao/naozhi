package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleLoginNoScript pins R243-SEC-15 (#800): the login form's
// non-JS POST fallback must be routed to a dedicated handler that
// drains the body without parsing it (so the token bytes never enter
// r.PostForm) and returns a clear "JavaScript required" page.
//
// Pre-fix the form's action="/dashboard" caused the form-encoded
// `token=…` body to be POSTed to the GET-only /dashboard handler,
// which returned 405 from ServeMux but left the request body in any
// upstream proxy / access log. The dedicated handler ensures the
// drain path is in our control and the response identifies why the
// browser cannot complete the no-JS submission.
func TestHandleLoginNoScript(t *testing.T) {
	t.Parallel()

	a := &AuthHandlers{
		dashboardToken: "secret",
		cookieSecret:   []byte("cookie"),
		loginLimiter:   newLoginLimiter(),
	}

	// A real no-JS browser submits the form url-encoded with the token
	// in the body. Confirm we (a) accept the request (drain instead of
	// 405), (b) never echo the token in the response, and (c) clearly
	// say JavaScript is required.
	body := "token=secret-must-not-leak&username=naozhi"
	r := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/auth/noscript",
		strings.NewReader(body))
	r.Host = "naozhi.example"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.handleLoginNoScript(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	resp := w.Body.String()
	if !strings.Contains(resp, "JavaScript required") {
		t.Fatalf("response missing JavaScript-required notice: %q", resp)
	}
	// Defence-in-depth: the handler must not echo the submitted token
	// in the response. A future templating mistake that interpolated
	// r.PostForm values would otherwise bounce the secret back to the
	// browser (and into any caching CDN downstream).
	if strings.Contains(resp, "secret-must-not-leak") {
		t.Fatalf("response leaked submitted token bytes: %q", resp)
	}
	// And the form value should not have been parsed into PostForm.
	// ParseForm was never called, so accessing PostForm yields nil
	// rather than a populated map. We test the observable contract:
	// Form is empty.
	if got := r.PostFormValue("token"); got != "" {
		t.Fatalf("PostFormValue(token) = %q; handler should not parse the body", got)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// TestLoginPageFormActionPointsAtNoScript pins the login HTML's form
// action attribute. Pre-fix this was action="/dashboard" which routed
// non-JS submissions to the GET-only dashboard handler. The fix moves
// the action to /api/auth/noscript so any future audit can grep for
// the explicit pairing form-action ↔ handler.
func TestLoginPageFormActionPointsAtNoScript(t *testing.T) {
	t.Parallel()
	if !strings.Contains(loginPageHTML, `action="/api/auth/noscript"`) {
		t.Fatal("loginPageHTML form action no longer points at /api/auth/noscript")
	}
	if strings.Contains(loginPageHTML, `action="/dashboard"`) {
		t.Fatal("loginPageHTML form action regressed to /dashboard (R243-SEC-15 #800)")
	}
}
