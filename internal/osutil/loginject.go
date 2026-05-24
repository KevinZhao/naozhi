package osutil

import (
	"strings"
	"unicode/utf8"
)

// IsLogInjectionRune reports whether r is a Unicode codepoint that would
// corrupt structured log output or terminal rendering when embedded in a
// user-supplied attribute. Covers:
//
//   - C1 controls (U+0080..U+009F). These encode as 2-byte UTF-8 starting
//     with 0xC2 (first byte >= 0x80), so a byte-level `r < 0x20 || r == 0x7f`
//     loop never catches them. Some terminals still interpret the legacy
//     ANSI C1 semantics (NEL, CSI, etc) and flip output.
//   - Bidi override / embedding (U+202A..U+202E). Let an attacker flip the
//     on-screen order of a log line under `tail -f` / `journalctl`, masking
//     fabricated content under the real entry. Also folded into
//     terminal emulators that render bidi.
//   - Bidi isolate (U+2066..U+2069). Same class as the overrides but newer
//     (Unicode 6.3). Ship-worthy log sanitizers must cover both.
//   - LS/PS (U+2028 / U+2029). Some JSON log consumers treat these as line
//     terminators → a single chat-ID or error string can split into two log
//     entries.
//
// Callers that also need to reject C0 controls (< 0x20) should gate on
// `r < 0x20 || r == 0x7f` separately — this helper intentionally targets
// only the class that byte-level filters miss.
//
// The policy mirrors internal/server/dashboard_cron.go (the original home
// of this function) and is the canonical source for any new code that
// needs to sanitize an attacker-influenced string before it reaches a
// slog attr or EventLog entry.
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
// corrupt structured log output or terminal rendering replaced by the
// single-byte literal "_". Caps the result at maxLen bytes; pass 0 to
// disable the cap.
//
// Rules:
//
//   - C0 controls and DEL: `< 0x20` or `== 0x7f` → "_". Includes tab (0x09)
//     because slog.TextHandler uses tab as the key/value separator; an
//     embedded tab would split one attr into two.
//   - Any rune flagged by IsLogInjectionRune → "_" (C1 / bidi / LS/PS).
//
// Intentionally lossy: this function is not for storing user-visible
// display content — it is for taking attacker-influenced strings (err.Error,
// chat-ID fragments, remote RPC error bodies) and rendering them safe as
// slog attribute values, EventLog system-event summaries, and similar log
// sinks. For display-quality sanitization that preserves CJK / emoji,
// call sites should continue to use their own policy.
//
// Fast path: when the input is already ASCII-clean and within the cap,
// returns s unchanged (no allocation). This is the common case — error
// messages from Go stdlib / our own `fmt.Errorf` wrappers are pure ASCII.
func SanitizeForLog(s string, maxLen int) string {
	if s == "" {
		return s
	}
	// Fast path: scan bytes. If every byte is ASCII-printable (0x20..0x7E)
	// return unchanged or — when oversized — slice directly. This covers
	// every error string produced by Go stdlib or our own Errorf wrappers.
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		// R243-GO-9: ASCII-clean superlong used to fall to strings.Map
		// just to truncate, walking every byte through the slow-path
		// mapper. Slice directly — every byte is single-rune ASCII so
		// the cap lands on a rune boundary without the rune-walk-back
		// the slow path needs (utf8.RuneStart loop below).
		if maxLen > 0 && len(s) > maxLen {
			return s[:maxLen]
		}
		return s
	}
	// Slow path: rewrite unsafe bytes/runes. Use strings.Map so we get
	// correct UTF-8 decoding for the bidi / LS/PS class.
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
		// Byte-level truncate, then walk back to the nearest rune
		// boundary so a multi-byte CJK character isn't split mid-rune
		// (which would emit an invalid-UTF-8 sequence into structured
		// log sinks).  The cap itself is a defense against oversized
		// attack strings.
		// Walk back to the nearest rune-start byte instead of calling
		// utf8.ValidString in a loop — utf8.ValidString is O(n) per call,
		// turning the truncation into O(n²) for adversarial multi-byte
		// suffixes. utf8.RuneStart(b) ↔ b&0xC0 != 0x80 identifies a UTF-8
		// continuation byte; at most 3 continuation bytes precede a rune
		// start, so this loop is O(1)…O(4). R244-GO-P3.
		mapped = mapped[:maxLen]
		for len(mapped) > 0 && !utf8.RuneStart(mapped[len(mapped)-1]) {
			mapped = mapped[:len(mapped)-1]
		}
	}
	return mapped
}
