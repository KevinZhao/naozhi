package dispatch

import (
	"strings"
	"testing"
)

// TestSanitizeCronDisplay_LinkSmuggling verifies R112714-ARCH-1: markdown
// link-syntax characters in Schedule/Prompt are replaced with full-width
// equivalents so a stored "[点我](http://evil)" cannot be echoed as a
// clickable IM link.
func TestSanitizeCronDisplay_LinkSmuggling(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string // must appear
		noASCII string // must NOT appear
	}{
		{
			name:    "link_in_prompt",
			input:   "[点我](http://evil.example)",
			wantSub: "点我",
			noASCII: "[点我](http://evil.example)",
		},
		{
			name:    "plain_ascii_schedule",
			input:   "0 * * * *",
			wantSub: "0 * * * *",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCronDisplay(tc.input, 60)
			if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
				t.Errorf("sanitizeCronDisplay(%q) = %q; want substring %q", tc.input, got, tc.wantSub)
			}
			if tc.noASCII != "" && strings.Contains(got, tc.noASCII) {
				t.Errorf("sanitizeCronDisplay(%q) = %q; must not contain raw link %q", tc.input, got, tc.noASCII)
			}
			if strings.ContainsAny(got, "[]()") {
				t.Errorf("sanitizeCronDisplay(%q) = %q; still contains ASCII []() chars", tc.input, got)
			}
		})
	}
}

// TestSanitizeCronDisplay_Truncation verifies rune-based truncation at maxRunes.
func TestSanitizeCronDisplay_Truncation(t *testing.T) {
	// 40 ASCII chars, limit 30 -> should be truncated
	long := strings.Repeat("a", 40)
	got := sanitizeCronDisplay(long, 30)
	runes := []rune(got)
	// After truncation the ellipsis is appended; result must be <= 31 runes
	if len(runes) > 31 {
		t.Errorf("truncated result %q has %d runes, want <= 31", got, len(runes))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("truncated result %q missing ellipsis", got)
	}
}

// TestSanitizeCronDisplay_StripNewlines verifies that embedded \n and \t are
// replaced with spaces so table rows aren't broken.
func TestSanitizeCronDisplay_StripNewlines(t *testing.T) {
	got := sanitizeCronDisplay("line1\nline2\ttab", 60)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("sanitizeCronDisplay still contains newline/tab: %q", got)
	}
}

// TestSanitizeCronDisplay_BidiStripped verifies bidi override runes are removed.
func TestSanitizeCronDisplay_BidiStripped(t *testing.T) {
	// U+202E RIGHT-TO-LEFT OVERRIDE
	got := sanitizeCronDisplay("safe‮text", 60)
	if strings.ContainsRune(got, '‮') {
		t.Errorf("sanitizeCronDisplay did not strip RLO rune: %q", got)
	}
}

// TestSanitizeCronDisplay_NoTruncationBelowLimit verifies short inputs are not modified.
func TestSanitizeCronDisplay_NoTruncationBelowLimit(t *testing.T) {
	input := "* * * * *"
	got := sanitizeCronDisplay(input, 30)
	if got != input {
		t.Errorf("sanitizeCronDisplay(%q) = %q; want unchanged", input, got)
	}
}
