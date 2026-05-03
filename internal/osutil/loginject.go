package osutil

import "strings"

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
	// and we're within the cap, return unchanged. This covers every error
	// string produced by Go stdlib or our own Errorf wrappers.
	clean := true
	if maxLen > 0 && len(s) > maxLen {
		clean = false
	} else {
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c < 0x20 || c == 0x7f || c >= 0x80 {
				clean = false
				break
			}
		}
	}
	if clean {
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
		// Byte-level truncate. The cap is a defense against oversized attack
		// strings (e.g. a 4 KB err.Error spewed into every log line); we do
		// not try to preserve rune boundaries because a truncated tail
		// character will either render or drop quietly depending on the
		// log sink, but neither path changes the injection surface.
		mapped = mapped[:maxLen]
	}
	return mapped
}
