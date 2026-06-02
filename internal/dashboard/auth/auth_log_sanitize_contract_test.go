package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRequireAuth_RejectLogSanitized is a source-level contract regression
// gate for R200109-SEC-2a.
//
// RequireAuth's "rejecting cross-origin mutating request" slog.Warn used to
// log r.URL.Path, r.Header.Get("Origin"), and r.Host raw. All three are
// attacker-controlled and can carry bidi / C1 / LS/PS code points that
// corrupt terminal log viewers or plant fake log entries.
//
// Contract: all three attrs MUST pass through osutil.SanitizeForLog.
func TestRequireAuth_RejectLogSanitized(t *testing.T) {
	t.Parallel()

	body := readHandlersSrc(t)

	const warn = `"rejecting cross-origin mutating request"`
	idx := strings.Index(body, warn)
	if idx < 0 {
		t.Fatalf(`handlers.go no longer contains %s slog.Warn; update this test.`, warn)
	}
	window := body[idx:]
	if len(window) > 512 {
		window = window[:512]
	}

	for _, want := range []string{
		"osutil.SanitizeForLog(r.URL.Path, 256)",
		`osutil.SanitizeForLog(r.Header.Get("Origin"), 256)`,
		"osutil.SanitizeForLog(r.Host, 256)",
	} {
		if !strings.Contains(window, want) {
			t.Errorf(`RequireAuth rejection warn must include %q (R200109-SEC-2a).`+
				"\nScanned window:\n%s", want, window)
		}
	}

	// Negative: raw unsanitized attrs must not appear.
	for _, bad := range []string{
		`"path", r.URL.Path,`,
		`"origin", r.Header.Get("Origin"),`,
		`"host", r.Host)`,
	} {
		if strings.Contains(window, bad) {
			t.Errorf(`handlers.go reintroduces unsanitized attr %q in RequireAuth rejection warn (R200109-SEC-2a).`, bad)
		}
	}
}

// TestHandleLogin_RejectLogSanitized is a source-level contract regression
// gate for R200109-SEC-2b.
//
// HandleLogin's "rejecting cross-origin login attempt" slog.Warn used to log
// r.Header.Get("Origin") and r.Host raw — same injection class as SEC-2a.
//
// Contract: both attrs MUST pass through osutil.SanitizeForLog.
func TestHandleLogin_RejectLogSanitized(t *testing.T) {
	t.Parallel()

	body := readHandlersSrc(t)

	const warn = `"rejecting cross-origin login attempt"`
	idx := strings.Index(body, warn)
	if idx < 0 {
		t.Fatalf(`handlers.go no longer contains %s slog.Warn; update this test.`, warn)
	}
	window := body[idx:]
	if len(window) > 256 {
		window = window[:256]
	}

	for _, want := range []string{
		`osutil.SanitizeForLog(r.Header.Get("Origin"), 256)`,
		"osutil.SanitizeForLog(r.Host, 256)",
	} {
		if !strings.Contains(window, want) {
			t.Errorf(`HandleLogin rejection warn must include %q (R200109-SEC-2b).`+
				"\nScanned window:\n%s", want, window)
		}
	}

	// Negative: raw unsanitized attrs must not appear.
	for _, bad := range []string{
		`"origin", r.Header.Get("Origin"),`,
		`"host", r.Host)`,
	} {
		if strings.Contains(window, bad) {
			t.Errorf(`handlers.go reintroduces unsanitized attr %q in HandleLogin rejection warn (R200109-SEC-2b).`, bad)
		}
	}
}

func readHandlersSrc(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "handlers.go"))
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	return string(src)
}
