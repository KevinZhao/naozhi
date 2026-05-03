package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestHandleResume_WorkspaceLogGoesThroughSanitizeForLog locks the
// R179-SEC-1 fix: `handleResume` previously logged a raw `workspace`
// attribute in its `resume workspace validation failed` slog.Warn. An
// authenticated dashboard caller could embed C1 / bidi / newline bytes
// in a non-existent workspace path to inject spoofed log entries —
// mirrors the R175-SEC-P1 fix on send.go but for the resume path.
//
// Contract: the attr must route through osutil.SanitizeForLog. This
// test uses source-level scanning so a future refactor that reverts
// to `"workspace", workspace` fails fast in CI.
func TestHandleResume_WorkspaceLogGoesThroughSanitizeForLog(t *testing.T) {
	t.Parallel()
	body := readDashboardSessionSource(t)

	// Negative: legacy unsanitized pattern must not reappear in this file.
	if strings.Contains(body, `"workspace", workspace)`) {
		t.Errorf("dashboard_session.go reintroduces the unsanitized workspace attr pattern.\n" +
			"R179-SEC-1 requires routing workspace through osutil.SanitizeForLog before passing it to slog.")
	}

	// Positive: the sanitized shape must appear somewhere.
	if !strings.Contains(body, "osutil.SanitizeForLog(workspace,") {
		t.Errorf("dashboard_session.go must route workspace through osutil.SanitizeForLog in the resume-validation log attr (R179-SEC-1).")
	}

	// Anchor the sanitized call to the resume-validation slog.Warn site
	// so a dummy sanitize elsewhere cannot paper over a revert.
	anchor := `"resume workspace validation failed"`
	idx := strings.Index(body, anchor)
	if idx < 0 {
		t.Fatalf("dashboard_session.go no longer contains the `%s` slog.Warn; update or remove this test.", anchor)
	}
	window := body[idx:]
	if len(window) > 512 {
		window = window[:512]
	}
	if !strings.Contains(window, "osutil.SanitizeForLog(workspace,") {
		t.Errorf("the `resume workspace validation failed` slog.Warn must feed workspace through osutil.SanitizeForLog.\n"+
			"Scanned window (first 512 bytes):\n%s", window)
	}
}

func readDashboardSessionSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(thisFile), "dashboard_session.go")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read dashboard_session.go: %v", err)
	}
	return string(data)
}
