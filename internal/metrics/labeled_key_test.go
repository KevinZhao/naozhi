package metrics

import (
	"strings"
	"testing"
)

// TestLabelKeySingleLabelFastPath asserts the single-label fast path is exactly
// equivalent to the general (pooled-builder) join for one label, including the
// empty, separator-collision, and oversized-segment cases. #2248.
func TestLabelKeySingleLabelFastPath(t *testing.T) {
	cases := []struct {
		name  string
		label string
		want  string
	}{
		{"plain", "kiro", "kiro"},
		{"empty", "", LabelEmpty},
		{"separator collision", "a|b", "a_b"},
		{"oversized clipped", strings.Repeat("x", maxLabelSegmentLen+10), strings.Repeat("x", maxLabelSegmentLen)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := labelKey([]string{tc.label})
			if got != tc.want {
				t.Fatalf("labelKey([%q]) = %q, want %q", tc.label, got, tc.want)
			}
			// The fast path must match clipLabelSegment, which is what the
			// general loop would produce for a single segment.
			if seg := clipLabelSegment(tc.label); got != seg {
				t.Fatalf("labelKey single-label %q diverged from clipLabelSegment %q", got, seg)
			}
		})
	}
}

// TestLabelKeySingleLabelAllocFree guards that the single-label fast path does
// not allocate (no pooled builder, no String() copy). #2248.
func TestLabelKeySingleLabelAllocFree(t *testing.T) {
	label := []string{"kiro"}
	allocs := testing.AllocsPerRun(100, func() {
		_ = labelKey(label)
	})
	if allocs != 0 {
		t.Fatalf("labelKey single-label allocated %.1f times/op, want 0", allocs)
	}
}
