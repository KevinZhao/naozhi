// Package textutil provides leaf-level string utilities shared across
// naozhi packages. It is a deliberate zero-dependency package — code that
// belongs here must be pure (no goroutines, no I/O, no logging) so any
// other internal package can consume it without inviting cycles.
//
// History: TruncateRunes and DeriveLegacyUUID lived in internal/cli prior
// to R-textutil. internal/discovery was forced to import cli purely to
// reach those helpers, forming a session → discovery → cli ← session
// diamond. Lifting the helpers into a leaf package severs the back-edge.
package textutil

import "unicode/utf8"

// TruncateRunes truncates s to at most maxRunes runes, appending "..." if
// the input was actually trimmed. Uses byte-level rune decoding to avoid
// allocating a full []rune slice on the hot path.
//
// Fast path: when len(s) <= maxRunes the byte length is already an upper
// bound on the rune count, so no truncation is possible. This short-
// circuit matters because TruncateRunes is called at ~5 events/s per
// active session for tool names and short summaries ("Read", "Write")
// that never need trimming — skipping the utf8 decode loop eliminates a
// steady CPU baseline.
func TruncateRunes(s string, maxRunes int) string {
	// maxRunes <= 0 is treated as "no limit": return s unchanged.
	// All production call sites pass positive constants; this guard prevents
	// an infinite loop if a misconfigured maxRunes ever reaches this function.
	if maxRunes <= 0 {
		return s
	}
	if len(s) <= maxRunes {
		return s
	}
	i, count := 0, 0
	for i < len(s) {
		if count == maxRunes {
			return s[:i] + "..."
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s
}
