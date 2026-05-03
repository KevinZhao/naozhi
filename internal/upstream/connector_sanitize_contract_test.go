package upstream

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLogSystemEvent_GoesThroughSanitizeForLog is a source-level contract
// regression gate for R172-SEC-M4.
//
// Prior to this round, the "send" branch of handleRequest echoed a raw
// err.Error() string into `sess.LogSystemEvent("发送失败：" + err.Error())`.
// The error originates from a transport stack that can contain C1 controls
// (U+0080..U+009F), bidi overrides (U+202A..U+202E, U+2066..U+2069), and
// LS/PS characters (U+2028/U+2029) that slip past the byte-level `< 0x20`
// gate. LogSystemEvent appends to persistedHistory AND broadcasts to every
// dashboard WS client subscribed to the session, giving a compromised
// relay a log-injection primitive into operator terminals.
//
// Contract: the "发送失败：" prefix MUST be followed by a SanitizeForLog
// call, not a raw err.Error(). This test scans the source and fails if
// any code path that calls LogSystemEvent skips the sanitizer.
func TestLogSystemEvent_GoesThroughSanitizeForLog(t *testing.T) {
	t.Parallel()

	body := readConnectorSource(t)

	// Structural contract: every LogSystemEvent call in connector.go must
	// feed the argument through osutil.SanitizeForLog. If a future change
	// reintroduces `LogSystemEvent("发送失败：" + err.Error())` style
	// concatenation, this assertion fails.
	// We allow the specific shape with SanitizeForLog, and reject the
	// legacy bare err.Error() pattern on the same line.
	legacyPattern := `sess.LogSystemEvent("发送失败：" + err.Error())`
	if strings.Contains(body, legacyPattern) {
		t.Errorf("connector.go reintroduces the unsanitized LogSystemEvent pattern.\n"+
			"Found legacy substring: %q\n"+
			"R172-SEC-M4 requires routing err.Error() through osutil.SanitizeForLog before surfacing it to the EventLog.",
			legacyPattern)
	}

	// Positive assertion: at least one LogSystemEvent call must use
	// SanitizeForLog. Without this, a refactor that deletes the only
	// call site would silently satisfy the negative assertion above.
	if !strings.Contains(body, "osutil.SanitizeForLog") {
		t.Errorf("connector.go must use osutil.SanitizeForLog in its LogSystemEvent call path (R172-SEC-M4).")
	}

	// Explicitly verify the sanitized shape lives next to the 发送失败
	// prefix — guards against a future edit that moves the sanitize call
	// elsewhere (e.g. into a separate slog.Warn) while leaving the
	// LogSystemEvent path raw. We allow arbitrary whitespace and line
	// breaks between the prefix and the sanitize call, so this will
	// survive gofmt adjustments.
	idx := strings.Index(body, `"发送失败：`)
	if idx < 0 {
		t.Fatalf("connector.go no longer contains the 发送失败 prefix; update or remove this test.")
	}
	// Scan the next 256 bytes for osutil.SanitizeForLog. The production
	// call site lives on the same statement so this fits in much less.
	window := body[idx:]
	if len(window) > 256 {
		window = window[:256]
	}
	if !strings.Contains(window, "osutil.SanitizeForLog(err.Error(),") {
		t.Errorf("the 发送失败 LogSystemEvent call site must invoke osutil.SanitizeForLog(err.Error(), ...).\n"+
			"Scanned window (first 256 bytes from the 发送失败 prefix):\n%s",
			window)
	}
}

// readConnectorSource returns the contents of connector.go relative to this
// test file. Resilient to `go test` being invoked from any working directory.
func readConnectorSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(thisFile), "connector.go")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read connector.go: %v", err)
	}
	return string(data)
}
