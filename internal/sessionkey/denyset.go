package sessionkey

// This file is the single source of truth for the codepoints a session key
// (or a component / log attr derived from one) may not contain; a contract
// test fails if the literals reappear outside sessionkey (#2301). Keys reach
// slog.TextHandler attrs, sessions.json and the shim argv / socket path, so:
// C0 (tab splits an attr, \n forges log lines), DEL + C1 (valid 2-byte UTF-8,
// missed by `< 0x20` gates), zero-width / bidi marks and BOM (invisible or
// direction-flipping), line / paragraph separators (newline to JSON log
// consumers). Deliberately differs from osutil.IsLogInjectionRune. The
// dashboard sanitizeKeySlug mirrors exactly this table (contract test in
// internal/server, #2429): grow both together.

// runeRange is one inclusive [lo, hi] codepoint range.
type runeRange struct{ lo, hi rune }

// controlKeyRuneRanges: C0 incl. tab / newline, then DEL + C1.
var controlKeyRuneRanges = [...]runeRange{
	{0x0000, 0x001F},
	{0x007F, 0x009F},
}

// invisibleKeyRuneRanges: zero-width / LTR-RTL marks, line / paragraph
// separator, bidi embedding / override, BOM. Sorted and disjoint from
// controlKeyRuneRanges so DeniedKeyRuneRanges is sorted as a whole.
var invisibleKeyRuneRanges = [...]runeRange{
	{0x200B, 0x200F},
	{0x2028, 0x2029},
	{0x202A, 0x202E},
	{0xFEFF, 0xFEFF},
}

func inRanges(r rune, ranges []runeRange) bool {
	for _, rg := range ranges {
		if r >= rg.lo && r <= rg.hi {
			return true
		}
	}
	return false
}

// IsControlKeyRune reports whether r is a C0 control (including tab and
// newline), DEL, or a C1 control.
func IsControlKeyRune(r rune) bool {
	return inRanges(r, controlKeyRuneRanges[:])
}

// IsInvisibleKeyRune reports whether r is one of the invisible / directional
// formatting codepoints a session key may not contain: U+200B..U+200F,
// U+2028, U+2029, U+202A..U+202E, U+FEFF. Adjacent codepoints are NOT included.
func IsInvisibleKeyRune(r rune) bool {
	return inRanges(r, invisibleKeyRuneRanges[:])
}

// IsForbiddenKeyRune reports whether r may not appear anywhere in a session
// key: IsControlKeyRune || IsInvisibleKeyRune. Rejecting and dropping callers
// share this predicate; callers wanting a replacement use SanitizeKeyRune.
func IsForbiddenKeyRune(r rune) bool {
	return IsControlKeyRune(r) || IsInvisibleKeyRune(r)
}

// SanitizeKeyRune maps every forbidden rune to '_' (never itself forbidden,
// never the ':' key separator) and returns any other rune unchanged. Shaped
// for strings.Map(sessionkey.SanitizeKeyRune, s).
func SanitizeKeyRune(r rune) rune {
	if IsForbiddenKeyRune(r) {
		return '_'
	}
	return r
}

// DeniedKeyRuneRanges returns the inclusive [lo, hi] ranges IsForbiddenKeyRune
// rejects, sorted and non-overlapping, as a fresh slice. Exposed for
// cross-layer contract tests; production code should call the predicates.
func DeniedKeyRuneRanges() [][2]rune {
	out := make([][2]rune, 0, len(controlKeyRuneRanges)+len(invisibleKeyRuneRanges))
	for _, rg := range controlKeyRuneRanges {
		out = append(out, [2]rune{rg.lo, rg.hi})
	}
	for _, rg := range invisibleKeyRuneRanges {
		out = append(out, [2]rune{rg.lo, rg.hi})
	}
	return out
}
