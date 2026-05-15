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
