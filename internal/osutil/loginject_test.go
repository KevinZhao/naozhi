package osutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestIsLogInjectionRune covers one rune from each class the godoc names
// (C1, bidi override, bidi isolate, LS/PS) and the content that must pass:
// ASCII, CJK and emoji.
func TestIsLogInjectionRune(t *testing.T) {
	t.Parallel()

	for _, r := range []rune{0x80, 0x85, 0x9F, 0x202A, 0x202E, 0x2066, 0x2069, 0x2028, 0x2029} {
		if !IsLogInjectionRune(r) {
			t.Errorf("IsLogInjectionRune(U+%04X) = false, want true", r)
		}
	}
	for _, r := range []rune{'A', ' ', '\n', 0x7f, '你', '😀', 0x200D} {
		if IsLogInjectionRune(r) {
			t.Errorf("IsLogInjectionRune(U+%04X) = true, want false", r)
		}
	}
}

// TestSanitizeForLog_RewritesControlBytes asserts every unsafe class is gone
// from the output while CJK and emoji survive unchanged.
func TestSanitizeForLog_RewritesControlBytes(t *testing.T) {
	t.Parallel()

	const unsafe = "\n\r\t\x7f\u0085\u202e\u2066\u2028\u2029"
	got := SanitizeForLog("a"+unsafe+"b", 0)
	if want := "a" + strings.Repeat("_", 9) + "b"; got != want {
		t.Errorf("SanitizeForLog rewrote to %q, want %q", got, want)
	}

	safe := "发送失败: context deadline exceeded 😞"
	if got := SanitizeForLog(safe, 0); got != safe {
		t.Errorf("SanitizeForLog(%q) = %q, want unchanged", safe, got)
	}
}

// TestSanitizeForLog_EnforcesMaxLen covers the byte cap: 0 disables it, the
// bound is inclusive, and rewriting happens before truncation.
func TestSanitizeForLog_EnforcesMaxLen(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 1000)
	if got := SanitizeForLog(long, 0); got != long {
		t.Errorf("cap=0 truncated to %d bytes", len(got))
	}
	if got := SanitizeForLog(strings.Repeat("a", 64), 64); len(got) != 64 {
		t.Errorf("len==cap: got %d bytes, want 64", len(got))
	}
	if got := SanitizeForLog(strings.Repeat("a", 65), 64); got != strings.Repeat("a", 64) {
		t.Errorf("len==cap+1: got %q", got)
	}
	got := SanitizeForLog(strings.Repeat("\n", 200), 64)
	if len(got) != 64 || strings.Contains(got, "\n") {
		t.Errorf("hostile+long: got %q (len %d)", got, len(got))
	}
}

// TestSanitizeForLog_CJKTruncationValidUTF8 is the regression test for #1943:
// a cap landing inside a multi-byte rune must never leave invalid UTF-8. The
// output feeds dashboard tool-chip titles and wshub broadcasts, not just logs.
func TestSanitizeForLog_CJKTruncationValidUTF8(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"中文世界", "ab世界", "x🌟🚀", "你a好b世c界"} {
		for maxLen := 0; maxLen <= len(in); maxLen++ {
			got := SanitizeForLog(in, maxLen)
			if !utf8.ValidString(got) {
				t.Errorf("SanitizeForLog(%q, %d) = %q produced invalid UTF-8", in, maxLen, got)
			}
			if maxLen > 0 && len(got) > maxLen {
				t.Errorf("SanitizeForLog(%q, %d) = %q exceeds cap", in, maxLen, got)
			}
		}
	}
	if got := SanitizeForLog("ab世界", 3); got != "ab" {
		t.Errorf("SanitizeForLog(%q, 3) = %q, want %q", "ab世界", got, "ab")
	}
}
