package cli

import (
	"strings"
	"testing"
)

// TestSanitizeStderrLine_TableDriven covers the four input classes the
// sanitizer is expected to handle correctly. R224-CR-8: prior coverage was
// implicit via readLoop end-to-end tests; the function is a security-sensitive
// log-injection defense (ANSI / C0 / bidi / OSC) and warrants its own
// table-driven suite so future edits cannot silently drop a class.
func TestSanitizeStderrLine_TableDriven(t *testing.T) {
	t.Parallel()

	const maxLen = maxStderrLogLineBytes

	cases := []struct {
		name string
		in   string
		want string
	}{
		// Fast path: pure ASCII printable + tab → returned unchanged, no
		// allocation. The implementation deliberately short-circuits here
		// because almost every CLI stderr line falls in this bucket.
		{"empty", "", ""},
		{"plain ASCII", "[ERROR] cli failed", "[ERROR] cli failed"},
		{"with tab", "key\tvalue", "key\tvalue"},

		// CSI escape (ESC [ ... final byte). Color codes used by chalk /
		// ink / Claude CLI when stderr is auto-detected as a TTY.
		{"CSI SGR red", "\x1b[31merror\x1b[0m", "error"},
		{"CSI cursor up", "\x1b[2Aprompt", "prompt"},

		// OSC sequence (ESC ] ... BEL). Window-title / hyperlink writes
		// would otherwise let stderr reposition the operator's terminal.
		{"OSC BEL terminator", "\x1b]0;title\x07tail", "tail"},
		{"OSC ST terminator", "\x1b]8;;link\x1b\\anchor\x1b]8;;\x1b\\", "anchor"},

		// Two-byte ESC fallback (anything that's not [ or ]). Older
		// terminal sequences like ESC = / ESC > .
		{"two-byte ESC", "ok\x1b=more", "okmore"},

		// C0 controls other than tab → dropped. \x07 (BEL) would beep
		// journalctl viewers; \x08 (BS) would erase preceding chars.
		{"BEL stripped", "ok\x07bar", "okbar"},
		{"BS stripped", "ok\x08bar", "okbar"},

		// C1 / bidi / LS-PS via IsLogInjectionRune. Each encodes as
		// multi-byte UTF-8 so the byte-level loop can't catch them; the
		// rune-decode arm is the one under test here.
		{"C1 NEL", "okbar", "okbar"},
		{"bidi RLO", "ok‮bar", "okbar"},
		{"bidi RLI", "ok⁧bar", "okbar"},
		{"line separator", "ok bar", "okbar"},
		{"para separator", "ok bar", "okbar"},

		// CJK / emoji must flow through unchanged — sanitizer is not
		// allowed to over-reject legitimate non-ASCII content.
		{"CJK passthrough", "错误：连接超时", "错误：连接超时"},
		{"emoji passthrough", "deploy ✅ done", "deploy ✅ done"},

		// Mixed ANSI + bidi: ANSI strip first, bidi rune drop second.
		{"ANSI + bidi", "\x1b[31m错‮误\x1b[0m", "错误"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeStderrLine(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeStderrLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Truncation: input longer than maxStderrLogLineBytes is pre-truncated
	// before the ANSI scanner so a multi-MB OSC payload cannot blow up the
	// strings.Builder. The implementation appends "…(truncated)" when the
	// cut fires; verify the output is bounded and carries the marker.
	t.Run("truncation_marker", func(t *testing.T) {
		long := strings.Repeat("a", maxLen+200)
		got := sanitizeStderrLine(long)
		if !strings.HasSuffix(got, "…(truncated)") {
			t.Errorf("sanitizeStderrLine truncation: missing marker, got suffix %q",
				got[max(0, len(got)-32):])
		}
		// Bounded length: pre-truncated input + marker. Allow a small
		// margin for the marker bytes.
		if len(got) > maxLen+len("…(truncated)") {
			t.Errorf("sanitizeStderrLine truncation: result %d bytes exceeds cap %d",
				len(got), maxLen+len("…(truncated)"))
		}
	})

	// UTF-8 boundary safety on truncation: cutting in the middle of a
	// multi-byte rune must back the cut up to a rune boundary, not produce
	// a malformed string.
	t.Run("truncation_utf8_boundary", func(t *testing.T) {
		// Build a string that places a 3-byte CJK rune across the cut.
		// "a" * (maxLen-1) + "中" pushes the second byte of "中" past the
		// cap; the implementation must back up to before "中".
		head := strings.Repeat("a", maxLen-1)
		got := sanitizeStderrLine(head + "中bar")
		if !strings.HasSuffix(got, "…(truncated)") {
			t.Fatalf("expected truncation marker, got %q", got)
		}
		// The truncated body must remain valid UTF-8 — strings.Builder
		// would otherwise emit replacement chars on a torn rune. We assert
		// no torn-byte 0xE4 (the leading byte of "中") appears as a
		// dangling tail before the marker.
		body := strings.TrimSuffix(got, "…(truncated)")
		if strings.ContainsRune(body, 0xFFFD) {
			t.Errorf("truncation tore a rune: body=%q", body)
		}
	})
}
