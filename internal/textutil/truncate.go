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

// TruncateRunesPair truncates s to two rune caps in a SINGLE UTF-8 scan,
// returning (lo, hi) where lo has at most loRunes and hi at most hiRunes,
// each with the same ellipsis behaviour as TruncateRunes. It is the fused
// equivalent of `TruncateRunes(s, loRunes), TruncateRunes(s, hiRunes)`.
//
// Event formatting derives both a short Summary (e.g. 120 runes) and a
// longer Detail (e.g. 2000 / 16000 runes) from the same assistant text or
// thinking block. Calling TruncateRunes twice rescans the same runes from
// the start; for multi-KB blocks at 5–50 events/s that doubles the decode
// work on a hot path. This walks the runes once, recording the lo cut while
// continuing to the hi cut. R20260602190132-PERF-11.
//
// Caps <= 0 mean "no limit" for that output (s returned unchanged), matching
// TruncateRunes. loRunes need not be <= hiRunes; each cut is independent.
func TruncateRunesPair(s string, loRunes, hiRunes int) (lo, hi string) {
	// Byte length bounds the rune count: if s already fits BOTH caps no
	// scan is possible to trim it. Each "no limit" (<=0) cap also fits.
	loFits := loRunes <= 0 || len(s) <= loRunes
	hiFits := hiRunes <= 0 || len(s) <= hiRunes
	if loFits && hiFits {
		return s, s
	}
	loIdx, hiIdx := -1, -1
	i, count := 0, 0
	for i < len(s) {
		if !loFits && loIdx < 0 && count == loRunes {
			loIdx = i
		}
		if !hiFits && hiIdx < 0 && count == hiRunes {
			hiIdx = i
		}
		if (loFits || loIdx >= 0) && (hiFits || hiIdx >= 0) {
			break
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	lo = cutWithEllipsis(s, loIdx)
	hi = cutWithEllipsis(s, hiIdx)
	return lo, hi
}

// cutWithEllipsis returns s[:idx] + ellipsis when idx >= 0 (a recorded
// truncation point), otherwise s unchanged. Mirrors the single-allocation
// pre-grown Builder fusion used by TruncateRunes.
func cutWithEllipsis(s string, idx int) string {
	if idx < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(idx + len(ellipsis))
	b.WriteString(s[:idx])
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
