package session

import "testing"

// TestDeniedKeyRuneRanges_PinsSet locks the codepoint classes
// ValidateSessionKey rejects. The dashboard's client-side sanitizeKeySlug
// is held to a superset of this table by a contract test in
// internal/server (#2429), so growing the table here without touching
// dashboard.js fails CI rather than shipping a client that emits keys the
// server 400s.
func TestDeniedKeyRuneRanges_PinsSet(t *testing.T) {
	t.Parallel()
	want := [][2]rune{
		{0x0000, 0x001F},
		{0x007F, 0x009F},
		{0x200B, 0x200F},
		{0x2028, 0x2029},
		{0x202A, 0x202E},
		{0xFEFF, 0xFEFF},
	}
	got := DeniedKeyRuneRanges()
	if len(got) != len(want) {
		t.Fatalf("DeniedKeyRuneRanges() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("range[%d] = %04X-%04X, want %04X-%04X", i, got[i][0], got[i][1], want[i][0], want[i][1])
		}
	}
	// Every rune in the table must actually be rejected by the validator,
	// and the table must be sorted + non-overlapping so range scans are
	// unambiguous.
	prevHi := rune(-1)
	for _, r := range got {
		if r[0] > r[1] || r[0] <= prevHi {
			t.Errorf("range %04X-%04X is inverted or overlaps/unsorted vs previous hi %04X", r[0], r[1], prevHi)
		}
		prevHi = r[1]
		for c := r[0]; c <= r[1]; c++ {
			if err := ValidateSessionKey("a:b:c" + string(c) + ":d"); err == nil {
				t.Errorf("ValidateSessionKey accepted U+%04X, which DeniedKeyRuneRanges lists", c)
			}
		}
	}
}

// TestDeniedKeyRuneRanges_ReturnsCopy guards the table against caller
// mutation: the exported accessor must hand out a fresh slice.
func TestDeniedKeyRuneRanges_ReturnsCopy(t *testing.T) {
	t.Parallel()
	a := DeniedKeyRuneRanges()
	a[0] = [2]rune{0x41, 0x41} // 'A'
	if err := ValidateSessionKey("A:b:c:d"); err != nil {
		t.Fatalf("mutating the returned slice leaked into the validator: %v", err)
	}
	if b := DeniedKeyRuneRanges(); b[0] != [2]rune{0x0000, 0x001F} {
		t.Fatalf("second call observed mutation: %v", b[0])
	}
}
