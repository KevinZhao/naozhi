package platform

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSplitTextWithCount_MatchesSplitText asserts the count-passing variant is
// behaviourally identical to SplitText across edge cases, so the #2283
// double-scan optimization in dispatch cannot diverge from the original.
func TestSplitTextWithCount_MatchesSplitText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		maxRune int
	}{
		{"empty", "", 10},
		{"under", "hello", 10},
		{"exact", "0123456789", 10},
		{"over_no_newline", strings.Repeat("a", 25), 10},
		{"over_with_newline", "line1\nline2\nline3\nline4\n", 12},
		{"cjk", strings.Repeat("中文", 30), 10},
		{"tiny_max", strings.Repeat("x", 50), 1},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want := SplitText(c.text, c.maxRune)
			got := SplitTextWithCount(c.text, c.maxRune, utf8.RuneCountInString(c.text))
			if len(got) != len(want) {
				t.Fatalf("chunk count = %d, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("chunk[%d] = %q, want %q", i, got[i], want[i])
				}
			}
		})
	}
}
