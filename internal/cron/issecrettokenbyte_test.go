package cron

import (
	"testing"
	"unicode"
)

// TestIsSecretTokenByte_EquivalentToUnicodeForm pins the ASCII byte-range
// rewrite (#1361 adjacent) to the exact set the prior unicode.IsDigit-based
// implementation accepted: for every byte value 0..255, the new direct
// comparisons must agree with the old (unicode.IsDigit(rune(b)) || a-z ||
// A-Z || - || _) form. This guards the equivalence argument that
// unicode.IsDigit only matches ASCII digits for single-byte inputs.
func TestIsSecretTokenByte_EquivalentToUnicodeForm(t *testing.T) {
	t.Parallel()

	oldForm := func(b byte) bool {
		r := rune(b)
		switch {
		case unicode.IsDigit(r):
			return true
		case r >= 'a' && r <= 'z':
			return true
		case r >= 'A' && r <= 'Z':
			return true
		case b == '-' || b == '_':
			return true
		default:
			return false
		}
	}

	for i := 0; i < 256; i++ {
		b := byte(i)
		if got, want := isSecretTokenByte(b), oldForm(b); got != want {
			t.Fatalf("byte %d (%q): isSecretTokenByte=%v, oldForm=%v", i, string(rune(b)), got, want)
		}
	}
}
