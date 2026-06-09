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

import (
	"strings"
	"unicode/utf8"
)

// ellipsis is the suffix appended by TruncateRunes / TruncateRunesBytes
// when the input is actually trimmed. Hoisted to a constant so the
// pre-grow Builder math reads as `i + len(ellipsis)` instead of a
// magic `+3`. R249-PERF-1.
const ellipsis = "..."

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
			// R249-PERF-1: `s[:i] + ellipsis` allocates twice (the concat
			// result and intermediate). A pre-grown strings.Builder fuses
			// both into a single backing slice; len math is exact so
			// Grow's amortised behaviour collapses to one allocation.
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
// returning (lo, hi) where lo = TruncateRunes(s, loRunes) and
// hi = TruncateRunes(s, hiRunes). Callers that derive both a short Summary
// and a longer Detail from the same source string would otherwise pay two
// full rune-decode passes over identical bytes; this fuses them into one.
// Requires 0 < loRunes <= hiRunes (the production Summary/Detail caps satisfy
// this); falls back to two independent TruncateRunes calls otherwise so the
// contract degrades safely. R20260602190132-PERF-11.
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
	// Consumed the whole string before hitting hiRunes: hi is untrimmed.
	// lo is trimmed iff we passed loRunes (loFound) before s ended.
	if loFound {
		return truncateAt(s, loCut), s
	}
	return s, s
}

// truncateAt returns s[:i]+ellipsis fused into a single allocation, mirroring
// the strings.Builder pre-grow in TruncateRunes.
func truncateAt(s string, i int) string {
	var b strings.Builder
	b.Grow(i + len(ellipsis))
	b.WriteString(s[:i])
	b.WriteString(ellipsis)
	return b.String()
}

// TruncateRunesNoEllipsis truncates s to at most maxRunes runes WITHOUT
// appending an ellipsis suffix. Used by IM card renderers (Feishu button
// labels, headers, tool_use_id values) where a trailing "..." would clutter
// the rendered card or — worse — push the result past the relay's own
// per-field length cap. The byte-level rune decode mirrors TruncateRunes so
// the only behavioural difference is the absent ellipsis. R219-CR-3.
//
// maxRunes <= 0 is treated as "no limit": return s unchanged. All call
// sites pass positive constants; this guard prevents an infinite loop if a
// misconfigured maxRunes ever reaches this function.
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
// ends on a rune boundary, or len(s) when s already fits within maxBytes.
// Returns 0 when s starts mid-codepoint (shouldn't happen for valid UTF-8).
//
// Use when a caller needs a byte-cap (not rune-cap) but must avoid splitting a
// multi-byte UTF-8 codepoint — e.g. /api/sessions resume last_prompt JSON
// fields and dashboard transcribe responses where the wire format is sized in
// bytes but garbled glyphs render as mojibake. Assumes s is valid UTF-8;
// callers in flow from strings.Map / osutil.SanitizeForLog satisfy this.
// R230-CQ-13.
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
// s[start:] begins on a rune boundary. Use when a caller wants a byte-sized
// tail of s (e.g. shortPath's "..." + p[len(p)-47:] mid-path truncation) but
// must not start the slice in the middle of a multi-byte UTF-8 codepoint,
// which would emit invalid UTF-8 into downstream JSON / dashboard fields.
// Clamps minStart into [0, len(s)]. Assumes s is valid UTF-8. R20260609-LB-2.
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

// TruncateRunesBytes mirrors TruncateRunes for a []byte input: it returns a
// string with at most maxRunes runes, appending "..." only when the input
// was actually trimmed. The conversion to string is deferred to the result
// (a byte-slice prefix or constructed truncated string) so callers passing
// large []byte payloads — e.g. cli.FormatToolInput's unknown-tool fallback
// dumping a multi-KB MCP tool-input json.RawMessage — avoid the full
// string(b) heap copy when truncation is the common case. R215-PERF-P2-6.
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
			// R249-PERF-1: same fusion as TruncateRunes — string(b[:i])
			// + ellipsis is two allocations, the Builder pre-grow is one.
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
