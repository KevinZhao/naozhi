package cron

import (
	"os"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/osutil"
)

// TestNotifyChatID_SanitizeForLog_StripsLogInjectionChars is the unit-level
// guard for R090135-LOGIC-3: osutil.SanitizeForLog must neutralize characters
// that an attacker-controlled chatID could use for log-line injection. The
// three notifyTarget slog.Warn call sites now wrap chatID with
// osutil.SanitizeForLog(chatID, 64).
func TestNotifyChatID_SanitizeForLog_StripsLogInjectionChars(t *testing.T) {
	t.Parallel()
	// These are the primary log-injection vectors: newlines and carriage
	// returns allow an attacker to forge additional log lines. Null bytes and
	// DEL (0x7f) can confuse log parsers. SanitizeForLog replaces them with '_'.
	cases := []struct {
		name    string
		input   string
		badByte byte // must not appear in output
	}{
		{name: "newline injection", input: "chatid\nfake_log_line=attacker", badByte: '\n'},
		{name: "carriage return", input: "chatid\roverwrite", badByte: '\r'},
		{name: "null byte", input: "chatid\x00suffix", badByte: 0x00},
		{name: "DEL byte", input: "chatid\x7fsuffix", badByte: 0x7f},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := osutil.SanitizeForLog(tc.input, 64)
			for i := 0; i < len(got); i++ {
				if got[i] == tc.badByte {
					t.Errorf("SanitizeForLog(%q): bad byte 0x%02x survived at index %d in %q",
						tc.input, tc.badByte, i, got)
				}
			}
		})
	}
}

// TestNotifyTarget_ChatID_SanitizeSourceAnchor is the source-anchor guard for
// R090135-LOGIC-3: the three slog.Warn call sites in notifyTarget that emit
// "chat", chatID must each pass chatID through osutil.SanitizeForLog rather
// than using the raw value. This mirrors the existing pattern in
// scheduler_run.go:741 (R20260607-SEC-1).
//
// Any future edit that removes the sanitizer from any of the three warn paths
// will fail this test without needing an end-to-end log-injection scenario.
func TestNotifyTarget_ChatID_SanitizeSourceAnchor(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_notify.go")
	if err != nil {
		t.Fatalf("read scheduler_notify.go: %v", err)
	}
	body := string(src)

	// Count how many times the notifyTarget function emits a "chat" slog key
	// with a raw chatID (not wrapped in SanitizeForLog).
	// A raw emission looks like: `"chat", chatID` without SanitizeForLog.
	rawCount := strings.Count(body, `"chat", chatID`)
	if rawCount > 0 {
		t.Errorf("scheduler_notify.go: found %d raw `\"chat\", chatID` slog emission(s); "+
			"all must use osutil.SanitizeForLog(chatID, 64) [R090135-LOGIC-3]", rawCount)
	}

	// Verify the sanitized form appears at least 3 times (the three Warn sites).
	sanitizedCount := strings.Count(body, `osutil.SanitizeForLog(chatID, 64)`)
	if sanitizedCount < 3 {
		t.Errorf("scheduler_notify.go: found %d osutil.SanitizeForLog(chatID, 64) call(s), want >=3 "+
			"(cap-drop Warn + deadline Warn + failure Warn) [R090135-LOGIC-3]", sanitizedCount)
	}
}
