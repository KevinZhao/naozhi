package sessionkey

// This file is the single source of truth for the codepoints a session key
// (or a session-key component / log attr derived from one) may not contain.
// Before R202606f-ARCH-6 (#2301) the same ranges were hand-copied into
// shim.validateKeyForShim, session.ValidateSessionKey,
// session.sanitizeKeyComponent and session.SanitizeQuote; each caller now
// applies its own policy (reject / map to '_' / drop) on top of the
// predicates below. A contract test in this package fails if any of these
// codepoint literals reappears outside sessionkey.
//
// Why the set is what it is — keys travel directly into slog.TextHandler
// attrs, sessions.json and the shim argv / socket path:
//
//   - C0 (U+0000..U+001F) incl. tab / newline: slog.TextHandler uses tab as
//     the key/value separator so an embedded tab fragments one attr into two;
//     \n injects fake log lines.
//   - DEL + C1 (U+007F..U+009F): some terminal emulators interpret C1
//     codepoints as control functions. They arrive as valid 2-byte UTF-8
//     (0xC2 0x80..0x9F) so byte-level `< 0x20` gates never catch them.
//   - U+200B..U+200F (zero-width space / joiner / LTR-RTL marks) and
//     U+FEFF (BOM): invisible; unsafe for human-readable log attrs.
//   - U+2028 / U+2029 (line / paragraph separator): treated as newlines by
//     some JSON log consumers → log-line injection.
//   - U+202A..U+202E (bidi embedding / override / pop): flip terminal
//     output direction, masking fabricated content under `tail -f` /
//     `journalctl`.
//
// Deliberately NOT the same set as osutil.IsLogInjectionRune: that helper
// includes the bidi isolates U+2066..U+2069 and excludes ZWSP / BOM (both
// test-pinned there). Do not widen this table to "match" it — the dashboard
// client-side sanitizeKeySlug is held to exactly this table by a contract
// test in internal/server (#2429), so any growth here must land together
// with a dashboard.js change.

// runeRange is one inclusive [lo, hi] codepoint range.
type runeRange struct{ lo, hi rune }

// controlKeyRuneRanges: C0 incl. tab / newline, then DEL + C1.
var controlKeyRuneRanges = [...]runeRange{
	{0x0000, 0x001F},
	{0x007F, 0x009F},
}

// invisibleKeyRuneRanges: zero-width / LTR-RTL marks, line / paragraph
// separator, bidi embedding / override, BOM. Sorted ascending and disjoint
// from controlKeyRuneRanges so DeniedKeyRuneRanges is sorted as a whole.
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
// newline), DEL, or a C1 control — the classes a byte-level `< 0x20` gate
// either catches or, for C1, silently misses.
func IsControlKeyRune(r rune) bool {
	return inRanges(r, controlKeyRuneRanges[:])
}

// IsInvisibleKeyRune reports whether r is one of the invisible / directional
// Unicode formatting codepoints a session key may not contain: U+200B..U+200F,
// U+2028, U+2029, U+202A..U+202E, U+FEFF. Adjacent codepoints (U+200A,
// U+2010, U+202F, U+2060, U+2066..U+2069, U+FEFE) are NOT in the set.
func IsInvisibleKeyRune(r rune) bool {
	return inRanges(r, invisibleKeyRuneRanges[:])
}

// IsForbiddenKeyRune reports whether r may not appear anywhere in a session
// key: IsControlKeyRune || IsInvisibleKeyRune. Callers that reject outright
// (session.ValidateSessionKey, shim.validateKeyForShim) and callers that
// drop the rune (session.SanitizeQuote) share this predicate; callers that
// want a replacement character use SanitizeKeyRune.
func IsForbiddenKeyRune(r rune) bool {
	return IsControlKeyRune(r) || IsInvisibleKeyRune(r)
}

// SanitizeKeyRune maps every forbidden rune to '_' and returns any other
// rune unchanged. Shaped for strings.Map:
//
//	strings.Map(sessionkey.SanitizeKeyRune, s)
//
// '_' is safe because it is never itself forbidden and never a key
// separator (':').
func SanitizeKeyRune(r rune) rune {
	if IsForbiddenKeyRune(r) {
		return '_'
	}
	return r
}

// DeniedKeyRuneRanges returns the inclusive [lo, hi] codepoint ranges that
// IsForbiddenKeyRune rejects, sorted ascending and non-overlapping, as a
// fresh slice so callers cannot mutate the table. Exposed for cross-layer
// contract tests (dashboard sanitizeKeySlug parity); production code should
// call the predicates above.
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
