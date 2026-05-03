package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSessionSend_WorkspaceLogGoesThroughSanitizeForLog is a source-level
// contract regression gate for R175-SEC-P1.
//
// Before this round, sessionSend logged `p.Workspace` raw in the
// `workspace validation failed` slog.Warn attr. ValidateSessionKey filters
// C1 / bidi / non-UTF-8 out of the KEY, but Workspace is a separate
// authenticated-user field that flows through validateWorkspace (filesystem
// semantics only) before landing in slog. A compromised dashboard / node
// client could inject `/valid/path<U+0085>attacker-line` to plant a fake
// log entry under `tail -f` / `journalctl` or flip terminal rendering via
// bidi override.
//
// Contract: the `slog.Warn("workspace validation failed"...)` call MUST
// feed p.Workspace through osutil.SanitizeForLog. This test locks the
// shape so a future edit that removes the sanitizer is caught in CI.
func TestSessionSend_WorkspaceLogGoesThroughSanitizeForLog(t *testing.T) {
	t.Parallel()

	body := readSendSource(t)

	// Negative: the raw bare `"workspace", p.Workspace` pattern must NOT
	// appear inside a slog.Warn attribute list. A refactor that reverts
	// to the pre-R175 shape will fail here.
	legacyPattern := `"workspace", p.Workspace`
	if strings.Contains(body, legacyPattern) {
		t.Errorf("send.go reintroduces the unsanitized workspace attr pattern.\n"+
			"Found legacy substring: %q\n"+
			"R175-SEC-P1 requires routing p.Workspace through osutil.SanitizeForLog before passing it to slog.",
			legacyPattern)
	}

	// Positive: at least one sanitized workspace log attr must live in
	// this file.
	if !strings.Contains(body, "osutil.SanitizeForLog(p.Workspace,") {
		t.Errorf("send.go must route p.Workspace through osutil.SanitizeForLog in the workspace-validation log attr (R175-SEC-P1).")
	}

	// Verify the sanitized shape is anchored to the "workspace validation
	// failed" slog.Warn call. Guards against a refactor that leaves a
	// dummy sanitize call elsewhere while reverting the real log site.
	idx := strings.Index(body, `"workspace validation failed"`)
	if idx < 0 {
		t.Fatalf("send.go no longer contains the `workspace validation failed` slog.Warn site; update or remove this test.")
	}
	window := body[idx:]
	if len(window) > 512 {
		window = window[:512]
	}
	if !strings.Contains(window, "osutil.SanitizeForLog(p.Workspace,") {
		t.Errorf("the `workspace validation failed` slog.Warn must pass p.Workspace through osutil.SanitizeForLog.\n"+
			"Scanned window (first 512 bytes from the slog.Warn call):\n%s",
			window)
	}
}

func readSendSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(thisFile), "send.go")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read send.go: %v", err)
	}
	return string(data)
}
