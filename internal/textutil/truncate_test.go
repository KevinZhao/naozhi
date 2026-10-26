package textutil

import (
	"testing"
	"unicode/utf8"
)

// TestTruncateRunes covers the fast path (byte length <= maxRunes), rune-aware
// truncation of multibyte input, and a 4-byte rune at the boundary.
func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"你好世界测试", 4, "你好世界..."},
		{"abcde", 5, "abcde"},
		{"🚀x", 1, "🚀..."},
	}
	for _, tc := range cases {
		if got := TruncateRunes(tc.in, tc.max); got != tc.want {
			t.Errorf("TruncateRunes(%q,%d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// TestTruncateRunesNoEllipsis covers the IM-card variant: identical truncation
// logic to TruncateRunes but with the trailing "..." suppressed so Feishu
// button labels stay clean. Mirrors the parametric coverage of TruncateRunes
// to prevent the two from diverging silently. R219-CR-3.
func TestTruncateRunesNoEllipsis(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short", "hello", 10, "hello"},
		{"truncated", "hello world", 5, "hello"},
		{"unicode", "你好世界测试", 4, "你好世界"},
		{"boundary_equal", "abcde", 5, "abcde"},
		{"single_rune", "🚀x", 1, "🚀"},
		{"empty", "", 5, ""},
		{"zero_max_no_limit", "hello", 0, "hello"},
		{"negative_max_no_limit", "hello", -1, "hello"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateRunesNoEllipsis(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("TruncateRunesNoEllipsis(%q,%d) = %q, want %q",
					tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// TestTruncateAtRuneBoundary pins the byte-cap helper used by dashboard
// resume / transcribe responses where the wire format sizes payloads in bytes
// but a mid-codepoint split would render as mojibake. R230-CQ-13.
func TestTruncateAtRuneBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     int
	}{
		{"short_returns_full_len", "hello", 10, 5},
		{"exact_returns_full_len", "hello", 5, 5},
		{"ascii_truncates_at_max", "hello world", 5, 5},
		{"unicode_walks_back_to_boundary", "你好世界", 4, 3},
		{"unicode_already_at_boundary", "你好世界", 6, 6},
		{"zero_max_returns_len", "hello", 0, 5},
		{"negative_max_returns_len", "hello", -1, 5},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateAtRuneBoundary(tc.in, tc.maxBytes)
			if got != tc.want {
				t.Errorf("TruncateAtRuneBoundary(%q,%d) = %d, want %d",
					tc.in, tc.maxBytes, got, tc.want)
			}
		})
	}
}

func TestTailAtRuneBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		minStart int
		want     int
	}{
		{"zero_returns_zero", "hello", 0, 0},
		{"negative_returns_zero", "hello", -3, 0},
		{"beyond_len_returns_len", "hello", 99, 5},
		{"ascii_exact_boundary", "hello", 2, 2},
		// "你好世界" = 4 runes x 3 bytes = 12 bytes. minStart=4 falls inside
		// the 2nd rune (bytes 3..5); walk forward to byte 6.
		{"cjk_walks_forward_to_boundary", "你好世界", 4, 6},
		{"cjk_already_on_boundary", "你好世界", 3, 3},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := TailAtRuneBoundary(tc.in, tc.minStart)
			if got != tc.want {
				t.Errorf("TailAtRuneBoundary(%q,%d) = %d, want %d",
					tc.in, tc.minStart, got, tc.want)
			}
			if got >= 0 && got <= len(tc.in) && !utf8.ValidString(tc.in[got:]) {
				t.Errorf("TailAtRuneBoundary(%q,%d) tail %q is invalid UTF-8",
					tc.in, tc.minStart, tc.in[got:])
			}
		})
	}
}

// TestTruncateRunesBytes mirrors TruncateRunes parametric cases against the
// []byte variant so the two helpers cannot diverge silently. R215-PERF-P2-6.
func TestTruncateRunesBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short", "hello", 10, "hello"},
		{"truncated", "hello world", 5, "hello..."},
		{"unicode", "你好世界测试", 4, "你好世界..."},
		{"boundary_equal", "abcde", 5, "abcde"},
		{"single_rune", "🚀x", 1, "🚀..."},
		{"empty", "", 5, ""},
		{"zero_max_no_limit", "hello", 0, "hello"},
		{"negative_max_no_limit", "hello", -1, "hello"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateRunesBytes([]byte(tc.in), tc.max)
			if got != tc.want {
				t.Errorf("TruncateRunesBytes(%q,%d) = %q, want %q",
					tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// TestTruncateRunesPair asserts the fused single-scan helper is behaviourally
// identical to two independent TruncateRunes calls across edge cases, and
// covers the degraded-contract fallbacks (lo>hi, non-positive caps).
// R20260602190132-PERF-11.
func TestTruncateRunesPair(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		in     string
		lo, hi int
	}{
		{"both_fit", "hello", 10, 20},
		{"lo_trims_hi_fits", "hello world here", 5, 100},
		{"both_trim", "hello world here we go", 5, 11},
		{"equal_caps", "hello world", 5, 5},
		{"unicode_both_trim", "你好世界测试一二三", 2, 5},
		{"unicode_lo_trim_hi_fit", "你好世界", 2, 100},
		{"exact_boundary_lo", "abcde", 5, 10},
		{"empty", "", 3, 8},
		{"lo_greater_than_hi_fallback", "hello world", 8, 3},
		{"zero_lo_fallback", "hello world", 0, 5},
		{"negative_hi_fallback", "hello world", 3, -1},
		{"single_rune_each", "🚀🛰️x", 1, 2},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotLo, gotHi := TruncateRunesPair(tc.in, tc.lo, tc.hi)
			wantLo := TruncateRunes(tc.in, tc.lo)
			wantHi := TruncateRunes(tc.in, tc.hi)
			if gotLo != wantLo {
				t.Errorf("TruncateRunesPair(%q,%d,%d) lo = %q, want %q",
					tc.in, tc.lo, tc.hi, gotLo, wantLo)
			}
			if gotHi != wantHi {
				t.Errorf("TruncateRunesPair(%q,%d,%d) hi = %q, want %q",
					tc.in, tc.lo, tc.hi, gotHi, wantHi)
			}
		})
	}
}
