package project

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandleFileGet_PublicTmpAuditLogSanitised verifies the public_tmp audit
// log line (R20260607-SEC-5) routes the served file path through
// osutil.SanitizeForLog so an attacker-influenced filename containing control
// bytes (ANSI escapes, NUL, newline) cannot inject into the log stream.
func TestHandleFileGet_PublicTmpAuditLogSanitised(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	h.publicTmpEnabled = true

	dir, err := os.MkdirTemp("/tmp", "naozhi-logsanitise-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// Filename embedding control bytes: ESC (ANSI) + newline (log split).
	// NUL is rejected by every filesystem so it can't be exercised here;
	// ESC/newline are accepted on Linux tmpfs/ext4. If the OS rejects them,
	// skip.
	fname := "evil\x1b[31m\nname.txt"
	fpath := filepath.Join(dir, fname)
	if err := os.WriteFile(fpath, []byte("payload\n"), 0o644); err != nil {
		t.Skipf("filesystem rejected control-byte filename: %v", err)
	}

	rel, err := filepath.Rel("/tmp", fpath)
	if err != nil {
		t.Fatal(err)
	}

	// Capture slog output.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	q := url.Values{}
	q.Set("project", publicTmpProject)
	q.Set("path", rel) // url.Values percent-encodes the control bytes
	q.Set("mode", "preview")
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	h.HandleFileGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 serving benign-content file, got %d body=%s", w.Code, w.Body.String())
	}

	logged := buf.String()
	if !strings.Contains(logged, "public_tmp file access") {
		t.Fatalf("audit log line not emitted; got: %q", logged)
	}
	// With SanitizeForLog the control bytes are rewritten to '_' BEFORE
	// reaching slog, so neither the raw ESC byte nor the TextHandler's escaped
	// form (\x1b) ever appears in the stream. Without the fix the TextHandler
	// quotes the raw ESC byte as \x1b, so asserting on that escaped
	// representation is load-bearing.
	for _, bad := range []string{"\x1b", `\x1b`} {
		if strings.Contains(logged, bad) {
			t.Errorf("audit log contains unsanitised control byte %q; log=%q", bad, logged)
		}
	}
	// The sanitised filename (control bytes -> '_') must be what got logged.
	if !strings.Contains(logged, "evil_") {
		t.Errorf("audit log missing sanitised filename marker; log=%q", logged)
	}
}
