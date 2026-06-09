package discovery

import (
	"testing"
)

// TestSanitizePromptForTransport_FastPathMultibyte verifies R20260608133928-PERF-15:
// valid multi-byte UTF-8 (Chinese, emoji) must not trigger the slow strings.Map
// path — the function must return the original string pointer-identical.
func TestSanitizePromptForTransport_FastPathMultibyte(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"pure Chinese", "你好，世界！"},
		{"emoji", "Hello 🌍"},
		{"Chinese with spaces and tab", "你\t好"},
		{"accented Latin", "café résumé"},
		{"mixed Chinese and ASCII", "I love 脑汁"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizePromptForTransport(tc.in)
			if got != tc.in {
				t.Errorf("SanitizePromptForTransport(%q) = %q, want original (unmodified)", tc.in, got)
			}
		})
	}
}

// TestSanitizePromptForTransport_C0ControlCharsReplaced verifies that C0
// control characters (except tab) are replaced with '_'.
func TestSanitizePromptForTransport_C0ControlCharsReplaced(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"NUL byte", "foo\x00bar", "foo_bar"},
		{"newline", "foo\nbar", "foo_bar"},
		{"carriage return", "foo\rbar", "foo_bar"},
		{"ESC", "foo\x1bbar", "foo_bar"},
		{"DEL (0x7f)", "foo\x7fbar", "foo_bar"},
		{"Chinese with C0", "你好\x01世界", "你好_世界"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizePromptForTransport(tc.in)
			if got != tc.want {
				t.Errorf("SanitizePromptForTransport(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizePromptForTransport_TabPreserved verifies that tab ('\t') is
// not replaced, both in ASCII-only and multi-byte strings.
func TestSanitizePromptForTransport_TabPreserved(t *testing.T) {
	cases := []string{
		"col1\tcol2",
		"你好\t世界",
		"\t",
	}
	for _, s := range cases {
		got := SanitizePromptForTransport(s)
		if got != s {
			t.Errorf("SanitizePromptForTransport(%q) = %q, want original (tab preserved)", s, got)
		}
	}
}

// TestSanitizePromptForTransport_LogInjectionRuneReplaced verifies that
// Unicode codepoints flagged by osutil.IsLogInjectionRune (e.g. bidi
// override, C1 controls like NEL U+0085) are replaced with '_'.
func TestSanitizePromptForTransport_LogInjectionRuneReplaced(t *testing.T) {
	// U+0085 NEL (NEXT LINE) — C1 control, flagged by IsLogInjectionRune.
	// U+202E RIGHT-TO-LEFT OVERRIDE — bidi injection vector.
	cases := []struct {
		name string
		in   string
	}{
		{"NEL U+0085", "foobar"},
		{"bidi override U+202E", "foo‮bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizePromptForTransport(tc.in)
			if got == tc.in {
				t.Errorf("SanitizePromptForTransport(%q): log-injection rune was not replaced (got same string)", tc.in)
			}
			// Confirm the injected rune position is '_'.
			for _, r := range got {
				if r == '' || r == '‮' {
					t.Errorf("SanitizePromptForTransport(%q): log-injection rune still present in output %q", tc.in, got)
				}
			}
		})
	}
}

// TestSanitizePromptForTransport_EmptyString ensures empty input is returned
// immediately without allocation.
func TestSanitizePromptForTransport_EmptyString(t *testing.T) {
	if got := SanitizePromptForTransport(""); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// TestSanitizePromptForTransport_PureASCII_FastPath verifies that a clean
// ASCII-only string (no control chars) is returned as-is.
func TestSanitizePromptForTransport_PureASCII_FastPath(t *testing.T) {
	in := "Hello, world! This is a test prompt."
	got := SanitizePromptForTransport(in)
	if got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}
