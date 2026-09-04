// Package textutil provides leaf-level string utilities shared across naozhi
// packages. It is deliberately zero-dependency: code here must be pure (no
// goroutines, I/O or logging) so any internal package can import it without
// cycles.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// ellipsis is the suffix appended when TruncateRunes / TruncateRunesBytes trim.
const ellipsis = "..."

// TruncateRunes truncates s to at most maxRunes runes, appending "..." if the
// input was actually trimmed. Byte-level rune decoding avoids allocating a
// []rune. When len(s) <= maxRunes the byte length already bounds the rune
// count, so the common short-token case skips the decode loop entirely.
func TruncateRunes(s string, maxRunes int) string {
	// maxRunes <= 0 means "no limit" and guards against an infinite loop.
	if maxRunes <= 0 {
		return s
	}
	if len(s) <= maxRunes {
		return s
	}
	i, count := 0, 0
	for i < len(s) {
		if count == maxRunes {
			// Pre-grown Builder fuses prefix + ellipsis into one allocation.
			var b strings.Builder
			b.Grow(i + len(ellipsis))
			b.WriteString(s[:i])
			b.WriteString(ellipsis)
			return b.String()
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s
}

// TruncateRunesPair truncates s to two rune limits in a single UTF-8 scan,
// returning (TruncateRunes(s, loRunes), TruncateRunes(s, hiRunes)) for
// callers deriving a short Summary and longer Detail from one string.
// Requires 0 < loRunes <= hiRunes; otherwise falls back to two independent
// TruncateRunes calls.
func TruncateRunesPair(s string, loRunes, hiRunes int) (lo, hi string) {
	if loRunes <= 0 || hiRunes <= 0 || loRunes > hiRunes {
		return TruncateRunes(s, loRunes), TruncateRunes(s, hiRunes)
	}
	// Byte length is an upper bound on rune count: when s fits within the
	// smaller cap it fits within the larger one too — no scan needed.
	if len(s) <= loRunes {
		return s, s
	}
	loCut, loFound := -1, false
	i, count := 0, 0
	for i < len(s) {
		if !loFound && count == loRunes {
			loCut = i
			loFound = true
		}
		if count == hiRunes {
			// Reached the larger cap mid-string: both need trimming.
			lo = truncateAt(s, loCut)
			hi = truncateAt(s, i)
			return lo, hi
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	// Whole string consumed before hiRunes: hi untrimmed; lo trimmed iff loFound.
	if loFound {
		return truncateAt(s, loCut), s
	}
	return s, s
}

// truncateAt returns s[:i]+ellipsis fused into a single allocation.
func truncateAt(s string, i int) string {
	var b strings.Builder
	b.Grow(i + len(ellipsis))
	b.WriteString(s[:i])
	b.WriteString(ellipsis)
	return b.String()
}

// TruncateRunesNoEllipsis truncates s to at most maxRunes runes WITHOUT an
// ellipsis, for IM card fields (Feishu button labels, headers, tool_use_id)
// where "..." would clutter the card or push past the relay's per-field cap.
// maxRunes <= 0 means "no limit".
func TruncateRunesNoEllipsis(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	if len(s) <= maxRunes {
		return s
	}
	i, count := 0, 0
	for i < len(s) {
		if count == maxRunes {
			return s[:i]
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s
}

// TruncateAtRuneBoundary returns the largest n <= maxBytes such that s[:n]
// ends on a rune boundary, or len(s) when s already fits. Returns 0 when s
// starts mid-codepoint. For byte-capped wire fields (JSON last_prompt,
// transcribe responses) that must not emit mojibake. Assumes valid UTF-8.
func TruncateAtRuneBoundary(s string, maxBytes int) int {
	if maxBytes <= 0 || maxBytes >= len(s) {
		return len(s)
	}
	for n := maxBytes; n > 0; n-- {
		if utf8.RuneStart(s[n]) {
			return n
		}
	}
	return 0
}

// TailAtRuneBoundary returns the smallest start index >= minStart such that
// s[start:] begins on a rune boundary, for byte-sized tails (mid-path "..."
// truncation) that must not start inside a multi-byte codepoint. Clamps
// minStart into [0, len(s)]. Assumes valid UTF-8.
func TailAtRuneBoundary(s string, minStart int) int {
	if minStart <= 0 {
		return 0
	}
	if minStart >= len(s) {
		return len(s)
	}
	for n := minStart; n < len(s); n++ {
		if utf8.RuneStart(s[n]) {
			return n
		}
	}
	return len(s)
}

// TruncateRunesBytes mirrors TruncateRunes for a []byte input. The string
// conversion is deferred to the result so callers passing large payloads
// (multi-KB MCP tool-input JSON) avoid a full string(b) copy when truncation
// is the common case.
func TruncateRunesBytes(b []byte, maxRunes int) string {
	if maxRunes <= 0 {
		return string(b)
	}
	if len(b) <= maxRunes {
		return string(b)
	}
	i, count := 0, 0
	for i < len(b) {
		if count == maxRunes {
			// Same one-allocation fusion as TruncateRunes.
			var sb strings.Builder
			sb.Grow(i + len(ellipsis))
			sb.Write(b[:i])
			sb.WriteString(ellipsis)
			return sb.String()
		}
		_, size := utf8.DecodeRune(b[i:])
		i += size
		count++
	}
	return string(b)
}
