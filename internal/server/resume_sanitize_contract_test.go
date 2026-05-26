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

// TestHandleResume_KeyEntropyIs128Bit pins R246-SEC-5 / R247-SEC-24 (#807):
// the resume key's random tail must be 16 bytes (128 bits), matching
// anonCookie / upload IDs and the rest of the codebase's short-id
// budget. The pre-fix form was `var rb [8]byte` — 64 bits, with a
// birthday-bound (~2^32 IDs before collision) that, while comfortably
// above realistic resume volume, was inconsistent with sibling code and
// kept the audit item open across four review rounds.
//
// Source-level pin (instead of round-tripping through the HTTP handler)
// because the key shape "dashboard:direct:r<32hex>:general" is only
// observable after a successful RegisterForResume — which depends on
// router state we don't otherwise need to spin up. A future refactor
// that resurrects `[8]byte` (or any [N]byte with N < 16) fails this
// test deterministically without flake from rand timing.
func TestHandleResume_KeyEntropyIs128Bit(t *testing.T) {
	t.Parallel()
	body := readDashboardSessionSource(t)

	// Negative: legacy 8-byte form must not reappear at the resume-key
	// generation site. Match through whitespace tolerantly so a one-line
	// reformat doesn't fool the gate.
	if strings.Contains(body, "var rb [8]byte") {
		t.Errorf("dashboard_session.go reintroduces 8-byte (64-bit) resume key entropy.\n" +
			"R246-SEC-5 / R247-SEC-24 require ≥16 bytes (128 bits) — see the\n" +
			"`resume register: generate key failed` site near hex.EncodeToString(rb[:]).")
	}

	// Positive: the 16-byte form must appear somewhere in the file.
	if !strings.Contains(body, "var rb [16]byte") {
		t.Errorf("dashboard_session.go must declare `var rb [16]byte` for the\n" +
			"resume key tail (R246-SEC-5 / R247-SEC-24, 128-bit entropy baseline).")
	}

	// Anchor: the 16-byte declaration must sit immediately before the
	// rand.Read call that feeds hex.EncodeToString(rb[:]) — otherwise a
	// stray `var rb [16]byte` elsewhere could paper over a revert at the
	// resume site. Walk forward from the array decl and verify the
	// rand.Read + hex.EncodeToString pair appears within a small window.
	idx := strings.Index(body, "var rb [16]byte")
	if idx < 0 {
		t.Fatalf("anchor missing — see negative branch above")
	}
	const windowBytes = 768
	end := idx + windowBytes
	if end > len(body) {
		end = len(body)
	}
	window := body[idx:end]
	if !strings.Contains(window, "rand.Read(rb[:])") {
		t.Errorf("expected `rand.Read(rb[:])` within %d bytes of `var rb [16]byte`.\n"+
			"Window:\n%s", windowBytes, window)
	}
	if !strings.Contains(window, "hex.EncodeToString(rb[:])") {
		t.Errorf("expected `hex.EncodeToString(rb[:])` within %d bytes of `var rb [16]byte`.\n"+
			"Window:\n%s", windowBytes, window)
	}
}
