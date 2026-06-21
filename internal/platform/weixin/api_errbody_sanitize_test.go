package weixin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// R202606c-SEC-1: a non-200 iLink response body is attacker-influenced and may
// carry C0/C1/bidi/control bytes. post must wrap it through SanitizeForLog
// before embedding it in the returned error string so it cannot poison the
// caller's structured logs / terminal rendering, and must truncate to 256.
func TestPost_SanitizesErrorBody(t *testing.T) {
	t.Parallel()

	// Body with embedded control bytes (NUL, tab, BEL, ESC) plus a long tail
	// to exercise the 256-char truncation.
	poison := "fail\x00\x07\x1b\ttail" + strings.Repeat("A", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(poison))
	}))
	defer srv.Close()

	// srv.URL is http://127.0.0.1:<port> → loopback, so no SSRF guard is
	// installed and the request reaches the test server.
	c := newAPIClient(srv.URL, "tok")
	_, err := c.post(context.Background(), "send", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	msg := err.Error()

	// No raw control byte may survive into the error string.
	for _, b := range []byte{0x00, 0x07, 0x1b, 0x09} {
		if strings.IndexByte(msg, b) >= 0 {
			t.Errorf("error string still contains raw control byte 0x%02x: %q", b, msg)
		}
	}
	// Sanitized output replaces control bytes with '_' and truncates the body
	// to 256 chars, so the full 1024-char tail must NOT appear verbatim.
	if strings.Contains(msg, strings.Repeat("A", 257)) {
		t.Errorf("error body was not truncated to 256 chars: %q", msg)
	}
	if !strings.Contains(msg, "http 502") {
		t.Errorf("error should still carry the status code, got %q", msg)
	}
}
