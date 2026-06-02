package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPprofRejectLog_PathSanitized is a source-level contract regression
// gate for R200109-SEC-1.
//
// The "rejecting non-loopback pprof request" slog.Warn used to log
// r.URL.Path raw. r.URL.Path is URL-decoded from the client-supplied
// request line and can carry bidi / C1 / LS/PS code points that corrupt
// terminal log viewers. debug_expvar.go already sanitizes its equivalent
// call; this test pins the pprof counterpart.
//
// Contract: the slog.Warn at the non-loopback rejection site MUST pass
// r.URL.Path through osutil.SanitizeForLog, never raw.
func TestPprofRejectLog_PathSanitized(t *testing.T) {
	t.Parallel()

	_, thisFile, _, _ := runtime.Caller(0)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "debug_pprof.go"))
	if err != nil {
		t.Fatalf("read debug_pprof.go: %v", err)
	}
	body := string(src)

	// Negative: the unsanitized raw pattern must not appear in any slog attr.
	if strings.Contains(body, `"path", r.URL.Path)`) {
		t.Errorf(`debug_pprof.go reintroduces unsanitized "path", r.URL.Path attr. ` +
			`R200109-SEC-1 requires osutil.SanitizeForLog(r.URL.Path, 256).`)
	}

	// Positive: sanitized form must be present.
	if !strings.Contains(body, "osutil.SanitizeForLog(r.URL.Path, 256)") {
		t.Errorf(`debug_pprof.go must pass r.URL.Path through osutil.SanitizeForLog(r.URL.Path, 256) ` +
			`in the non-loopback rejection slog.Warn (R200109-SEC-1).`)
	}

	// Anchor: sanitized form must appear within the rejection warn.
	idx := strings.Index(body, `"rejecting non-loopback pprof request"`)
	if idx < 0 {
		t.Fatal(`debug_pprof.go no longer contains the "rejecting non-loopback pprof request" slog.Warn; update this test.`)
	}
	window := body[idx:]
	if len(window) > 256 {
		window = window[:256]
	}
	if !strings.Contains(window, "osutil.SanitizeForLog(r.URL.Path, 256)") {
		t.Errorf(`the "rejecting non-loopback pprof request" slog.Warn must use osutil.SanitizeForLog for the path attr.`+
			"\nScanned window:\n%s", window)
	}
}
