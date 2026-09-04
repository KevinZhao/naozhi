package osutil

import (
	"strings"
	"unicode/utf8"
)

// IsLogInjectionRune reports whether r is a Unicode codepoint that corrupts
// structured log output or terminal rendering when embedded in a
// user-supplied attribute: C1 controls (U+0080..U+009F, which a byte-level
// `r < 0x20` filter misses because they encode as two UTF-8 bytes), bidi
// override/embedding (U+202A..U+202E) and isolate (U+2066..U+2069) marks that
// can reorder a log line under `tail -f` / `journalctl`, and LS/PS
// (U+2028/U+2029), which some JSON log consumers treat as line terminators.
//
// C0 controls are NOT covered; callers gate `r < 0x20 || r == 0x7f`
// separately. Canonical policy for sanitizing attacker-influenced strings.
func IsLogInjectionRune(r rune) bool {
	switch {
	case r >= 0x80 && r <= 0x9F: // C1 controls
		return true
	case r >= 0x202A && r <= 0x202E: // LRE/RLE/PDF/LRO/RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI/RLI/FSI/PDI
		return true
	case r == 0x2028 || r == 0x2029: // LS/PS
		return true
	}
	return false
}

// SanitizeForLog returns a copy of s with every byte or rune that would
// corrupt structured log output or terminal rendering replaced by "_":
// C0 controls and DEL (including tab, slog.TextHandler's key/value
// separator) and everything IsLogInjectionRune flags. Caps the result at
// maxLen bytes; 0 disables the cap.
//
// Intentionally lossy: for attacker-influenced strings (err.Error, chat-ID
// fragments, remote RPC error bodies) headed to slog attrs / EventLog, not
// for user-visible display. ASCII-clean input within the cap is returned
// unchanged without allocation.
func SanitizeForLog(s string, maxLen int) string {
	if s == "" {
		return s
	}
	// Fast path: ASCII-printable input needs no rewriting.
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		// Every byte is single-rune ASCII, so a byte cap lands on a rune boundary.
		if maxLen > 0 && len(s) > maxLen {
			return s[:maxLen]
		}
		return s
	}
	// Slow path: strings.Map decodes UTF-8 correctly for the bidi / LS/PS class.
	mapped := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		if IsLogInjectionRune(r) {
			return '_'
		}
		return r
	}, s)
	if maxLen > 0 && len(mapped) > maxLen {
		// Byte-level truncate, then walk back over UTF-8 continuation bytes so a
		// multi-byte rune is not split (utf8.RuneStart is O(1)..O(4) per cap;
		// utf8.ValidString in a loop would be O(n²) on adversarial input).
		mapped = mapped[:maxLen]
		for len(mapped) > 0 && !utf8.RuneStart(mapped[len(mapped)-1]) {
			mapped = mapped[:len(mapped)-1]
		}
		// The walk-back stops on a lead byte (RuneStart is true for leads), which
		// leaves an incomplete rune if the cap fell right after it; an incomplete
		// trailing rune decodes as (RuneError, 1), so drop that byte too (#1943).
		if r, size := utf8.DecodeLastRuneInString(mapped); r == utf8.RuneError && size == 1 {
			mapped = mapped[:len(mapped)-1]
		}
	}
	return mapped
}
