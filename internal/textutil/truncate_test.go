package textutil

import "testing"

func TestTruncateRunes_Short(t *testing.T) {
	t.Parallel()
	got := TruncateRunes("hello", 10)
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTruncateRunes_Truncated(t *testing.T) {
	t.Parallel()
	got := TruncateRunes("hello world", 5)
	if got != "hello..." {
		t.Errorf("got %q, want %q", got, "hello...")
	}
}

func TestTruncateRunes_Unicode(t *testing.T) {
	t.Parallel()
	got := TruncateRunes("你好世界测试", 4)
	if got != "你好世界..." {
		t.Errorf("got %q, want %q", got, "你好世界...")
	}
}

// TestTruncateRunes_BoundaryEqual ensures a string whose byte-length equals
// maxRunes (no multibyte) takes the fast path and returns unchanged.
func TestTruncateRunes_BoundaryEqual(t *testing.T) {
	t.Parallel()
	got := TruncateRunes("abcde", 5)
	if got != "abcde" {
		t.Errorf("got %q, want %q", got, "abcde")
	}
}

// TestTruncateRunes_SingleRune covers the rune-count = 1 + truncation path
// where a 4-byte rune ("🚀") and maxRunes=0 forces the truncation branch.
func TestTruncateRunes_SingleRune(t *testing.T) {
	t.Parallel()
	got := TruncateRunes("🚀x", 1)
	if got != "🚀..." {
		t.Errorf("got %q, want %q", got, "🚀...")
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
