package sessionkey_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// invisibleSet is every codepoint IsInvisibleKeyRune must accept-as-forbidden,
// enumerated one by one (not as ranges) so a range typo in the production
// table cannot be mirrored here.
var invisibleSet = []rune{
	0x200B, 0x200C, 0x200D, 0x200E, 0x200F, // ZWSP ZWNJ ZWJ LRM RLM
	0x2028, 0x2029, // LS PS
	0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // LRE RLE PDF LRO RLO
	0xFEFF, // BOM
}

// invisibleNeighbours are codepoints adjacent to (or commonly confused with)
// the invisible set that must stay allowed. U+2060 (word joiner) and the bidi
// isolates U+2066..U+2069 are in gitinfo / osutil deny-sets but deliberately
// NOT in the session-key set — see denyset.go.
var invisibleNeighbours = []rune{
	0x200A,                         // hair space, just below ZWSP
	0x2010,                         // hyphen, just above RLM
	0x2027,                         // hyphenation point, just below LS
	0x202F,                         // narrow no-break space, just above RLO
	0x2060,                         // word joiner
	0x2066, 0x2067, 0x2068, 0x2069, // bidi isolates
	0xFEFE, 0xFF00,
	' ', ':', '_', 'a', '~', 0x00A0, '中', '😀', utf8.RuneError,
}

func TestIsInvisibleKeyRune_ExactSet(t *testing.T) {
	t.Parallel()
	for _, r := range invisibleSet {
		if !sessionkey.IsInvisibleKeyRune(r) {
			t.Errorf("IsInvisibleKeyRune(U+%04X) = false, want true", r)
		}
		if sessionkey.IsControlKeyRune(r) {
			t.Errorf("IsControlKeyRune(U+%04X) = true; invisible runes are not controls", r)
		}
	}
	for _, r := range invisibleNeighbours {
		if sessionkey.IsInvisibleKeyRune(r) {
			t.Errorf("IsInvisibleKeyRune(U+%04X) = true, want false (outside the set)", r)
		}
	}
}

func TestIsControlKeyRune_Boundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		r    rune
		want bool
	}{
		{0x0000, true}, {0x0008, true}, {'\t', true}, {'\n', true}, {'\r', true}, {0x001F, true},
		{0x0020, false}, {0x007E, false},
		{0x007F, true}, {0x0080, true}, {0x0085, true}, {0x009F, true},
		{0x00A0, false}, {'a', false}, {'中', false},
		{-1, false},
	}
	for _, c := range cases {
		if got := sessionkey.IsControlKeyRune(c.r); got != c.want {
			t.Errorf("IsControlKeyRune(%#x) = %v, want %v", c.r, got, c.want)
		}
	}
}

// TestIsForbiddenKeyRune_UnionAndCardinality sweeps the whole Unicode space:
// the union predicate must agree with its two halves and with the exported
// range table, and the total number of forbidden codepoints is pinned so the
// set cannot silently grow or shrink (32 C0 + 33 DEL/C1 + 5 + 2 + 5 + 1).
func TestIsForbiddenKeyRune_UnionAndCardinality(t *testing.T) {
	t.Parallel()
	ranges := sessionkey.DeniedKeyRuneRanges()
	inTable := func(r rune) bool {
		for _, rg := range ranges {
			if r >= rg[0] && r <= rg[1] {
				return true
			}
		}
		return false
	}
	count := 0
	for r := rune(0); r <= utf8.MaxRune; r++ {
		got := sessionkey.IsForbiddenKeyRune(r)
		if want := sessionkey.IsControlKeyRune(r) || sessionkey.IsInvisibleKeyRune(r); got != want {
			t.Fatalf("IsForbiddenKeyRune(U+%04X) = %v, control||invisible = %v", r, got, want)
		}
		if got != inTable(r) {
			t.Fatalf("IsForbiddenKeyRune(U+%04X) = %v, DeniedKeyRuneRanges membership = %v", r, got, !got)
		}
		if got {
			count++
		}
	}
	const want = 32 + 33 + 5 + 2 + 5 + 1
	if count != want {
		t.Fatalf("forbidden codepoint count = %d, want %d — the deny-set changed size", count, want)
	}
	if sessionkey.IsForbiddenKeyRune(-1) {
		t.Fatal("IsForbiddenKeyRune(-1) = true, want false")
	}
}

func TestSanitizeKeyRune(t *testing.T) {
	t.Parallel()
	for _, r := range invisibleSet {
		if got := sessionkey.SanitizeKeyRune(r); got != '_' {
			t.Errorf("SanitizeKeyRune(U+%04X) = %q, want '_'", r, got)
		}
	}
	for _, r := range []rune{0x00, '\t', '\n', 0x1F, 0x7F, 0x80, 0x9F} {
		if got := sessionkey.SanitizeKeyRune(r); got != '_' {
			t.Errorf("SanitizeKeyRune(%#x) = %q, want '_'", r, got)
		}
	}
	for _, r := range invisibleNeighbours {
		if got := sessionkey.SanitizeKeyRune(r); got != r {
			t.Errorf("SanitizeKeyRune(U+%04X) = U+%04X, want identity", r, got)
		}
	}
	// strings.Map shape: replacement is 1:1 per rune and the output is clean.
	in := "a\u200Bb:c\td\u202Ee\uFEFF"
	got := strings.Map(sessionkey.SanitizeKeyRune, in)
	if want := "a_b:c_d_e_"; got != want {
		t.Fatalf("strings.Map(SanitizeKeyRune, %q) = %q, want %q", in, got, want)
	}
	for _, r := range got {
		if sessionkey.IsForbiddenKeyRune(r) {
			t.Fatalf("sanitized output still contains forbidden U+%04X", r)
		}
	}
}

// TestDeniedKeyRuneRanges_PinsTable locks the exact table (values AND order):
// the dashboard's sanitizeKeySlug is held to this table by a contract test in
// internal/server (#2429), and session.DeniedKeyRuneRanges delegates here.
func TestDeniedKeyRuneRanges_PinsTable(t *testing.T) {
	t.Parallel()
	want := [][2]rune{
		{0x0000, 0x001F},
		{0x007F, 0x009F},
		{0x200B, 0x200F},
		{0x2028, 0x2029},
		{0x202A, 0x202E},
		{0xFEFF, 0xFEFF},
	}
	got := sessionkey.DeniedKeyRuneRanges()
	if len(got) != len(want) {
		t.Fatalf("DeniedKeyRuneRanges() = %v, want %v", got, want)
	}
	prevHi := rune(-1)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("range[%d] = %04X-%04X, want %04X-%04X", i, got[i][0], got[i][1], want[i][0], want[i][1])
		}
		if got[i][0] > got[i][1] || got[i][0] <= prevHi {
			t.Errorf("range[%d] %04X-%04X is inverted or unsorted/overlapping vs previous hi %04X", i, got[i][0], got[i][1], prevHi)
		}
		prevHi = got[i][1]
	}
}

func TestDeniedKeyRuneRanges_ReturnsCopy(t *testing.T) {
	t.Parallel()
	a := sessionkey.DeniedKeyRuneRanges()
	a[0] = [2]rune{'A', 'A'}
	if sessionkey.IsForbiddenKeyRune('A') {
		t.Fatal("mutating the returned slice leaked into the predicate")
	}
	if b := sessionkey.DeniedKeyRuneRanges(); b[0] != [2]rune{0x0000, 0x001F} {
		t.Fatalf("second call observed mutation: %v", b[0])
	}
}
