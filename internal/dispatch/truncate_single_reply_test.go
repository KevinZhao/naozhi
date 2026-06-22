package dispatch

import (
	"testing"
	"unicode/utf8"
)

// oldTruncateForSingleReply is the pre-R202606e-PERF-001 implementation kept
// here as the equivalence oracle: the zero-alloc TruncateRunesNoEllipsis path
// must produce byte-for-byte identical output across both the marker-fits and
// marker-dropped branches, for ASCII and multi-byte unicode text.
func oldTruncateForSingleReply(text string, maxRunes int) string {
	markerRunes := utf8.RuneCountInString(singleReplyTruncMarker)
	keep := maxRunes - markerRunes
	if keep <= 0 {
		return string([]rune(text)[:maxRunes])
	}
	return string([]rune(text)[:keep]) + singleReplyTruncMarker
}

func TestTruncateForSingleReply_EquivalentToRuneSlice(t *testing.T) {
	t.Parallel()
	markerRunes := utf8.RuneCountInString(singleReplyTruncMarker)
	cases := []struct {
		name    string
		text    string
		maxRune int
	}{
		// marker fits (keep > 0)
		{"ascii_marker_fits", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJ", markerRunes + 5},
		{"unicode_marker_fits", "一二三四五六七八九十甲乙丙丁戊己庚辛壬癸子丑寅卯", markerRunes + 4},
		{"mixed_marker_fits", "héllo世界café日本語テストあいうえおか", markerRunes + 6},
		// marker does NOT fit (keep <= 0): maxRunes <= markerRunes, so the bare
		// truncation keeps exactly maxRunes runes of content.
		{"ascii_marker_dropped", "abcdefghijklmnop", markerRunes - 1},
		{"unicode_marker_dropped", "一二三四五六七八九十甲乙丙丁", markerRunes - 2},
		{"marker_dropped_exact", "αβγδεζηθικλμνξο", markerRunes},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// Guard the test fixture: the original []rune[:n] panics if n >
			// len(runes); callers only invoke this when text is oversized so
			// maxRunes < rune length. Keep fixtures honouring that contract.
			if utf8.RuneCountInString(c.text) < c.maxRune {
				t.Fatalf("fixture invariant broken: text has %d runes < maxRunes %d",
					utf8.RuneCountInString(c.text), c.maxRune)
			}
			want := oldTruncateForSingleReply(c.text, c.maxRune)
			got := truncateForSingleReply(c.text, c.maxRune)
			if got != want {
				t.Errorf("truncateForSingleReply(%q, %d) = %q; want %q (byte mismatch)",
					c.text, c.maxRune, got, want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: %q", got)
			}
			if n := utf8.RuneCountInString(got); n > c.maxRune {
				t.Errorf("result has %d runes, exceeds maxRunes %d", n, c.maxRune)
			}
		})
	}
}
