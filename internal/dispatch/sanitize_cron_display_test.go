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

// Newlines and tabs become spaces (table rows stay intact), bidi overrides
// are stripped, and short plain input passes unchanged.
func TestSanitizeCronDisplay_ControlRunes(t *testing.T) {
	if got := sanitizeCronDisplay("line1\nline2\ttab", 60); got != "line1 line2 tab" {
		t.Errorf("newline/tab: got %q", got)
	}
	if got := sanitizeCronDisplay("safe\u202etext", 60); strings.ContainsRune(got, '\u202e') {
		t.Errorf("did not strip RLO rune: %q", got)
	}
	if got := sanitizeCronDisplay("* * * * *", 30); got != "* * * * *" {
		t.Errorf("short input changed: %q", got)
	}
}
