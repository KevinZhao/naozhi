package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleDashboardNoScript_RejectsPostWithoutReadingBody pins R243-SEC-15
// (#800). The login form's `action="/dashboard"` POST fallback path (when JS
// is disabled) must:
//
//  1. Return a deterministic 4xx so the browser surfaces an error.
//  2. NEVER read r.Body — even though the body is HTTPS-encrypted in flight,
//     anything the handler reads off the body could be echoed into slog
//     attrs or proxied error pages by future regressions. The whole point
//     of routing this to its own handler (rather than letting Go's mux
//     synthesise a 405) is to make the no-read invariant testable.
//  3. NEVER echo any submitted bytes (token / username) back in the response
//     body — the response must be a static literal.
//  4. Set Allow: GET so an HTTP-aware client / debugger can see the contract
//     immediately without parsing prose.
//
// Asserting these four together prevents a future "let's parse the form so
// we can show a nicer error" regression that would re-introduce the very
// risk the explicit handler exists to mitigate.
func TestHandleDashboardNoScript_RejectsPostWithoutReadingBody(t *testing.T) {
	t.Parallel()

	s := &Server{}

	// Token that MUST NOT appear in the response body. We use a sentinel
	// substring that's unlikely to collide with any error / template text.
	const sentinel = "this-token-must-not-leak-back-7c91f"
	body := "username=naozhi&token=" + sentinel
	// trackingReader counts bytes the handler reads. The handler is
	// supposed to return without touching r.Body at all, so any non-zero
	// count is a regression. We can't probe r.Body.Close calls because
	// the http server may close the body itself, so byte-count is the
	// load-bearing assertion.
	tr := &trackingReader{r: strings.NewReader(body)}
	r := httptest.NewRequest(http.MethodPost, "/dashboard", tr)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	s.handleDashboardNoScript(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow header = %q, want %q", got, "GET")
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}
	if tr.bytesRead != 0 {
		t.Errorf("handler read %d bytes from r.Body, want 0 — body must NOT be touched on the no-JS fallback path", tr.bytesRead)
	}
	if strings.Contains(w.Body.String(), sentinel) {
		t.Errorf("response body contains submitted token bytes %q — handler must never echo body data", sentinel)
	}
}

// trackingReader counts bytes read so the test can assert the handler did
// not consume any of the request body.
type trackingReader struct {
	r         io.Reader
	bytesRead int
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.bytesRead += n
	return n, err
}
