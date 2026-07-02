// Package leakguard is the single source of truth for detecting a "leaked
// tool call" — the case where the model regresses and writes tool-call XML as
// plain PROSE into an assistant text turn instead of emitting a structured
// tool_use content block, e.g.
//
//	call
//	<invoke name="Bash">
//	<parameter name="command">…</parameter>
//	</invoke>
//
// Because it is text (not a real tool_use), nothing executes, the CLI emits a
// normal end_turn result, and the turn stalls — the model believes its work is
// unfinished but the system considers the turn complete.
//
// The detection anchor is kept byte-for-byte in lockstep with
// LEAKED_TOOLCALL_RE in internal/server/static/dashboard.js (the frontend
// cosmetic fold). internal/server/static_leaked_toolcall_test.go asserts the
// JS literal still equals Anchor, so JS ⇄ Go ⇄ runtime cannot drift.
//
// The anchor is deliberately STRICT: it requires a `call` or `<function_calls>`
// marker alone on its own line immediately preceding `<invoke name="`. A bare
// `<invoke …>` quoted in backticks while discussing tool syntax must NOT match.
// Validated on 667 real events: 9 genuine leaks caught, 0 false positives.
package leakguard

import (
	"regexp"
	"strings"
)

// Anchor mirrors LEAKED_TOOLCALL_RE in dashboard.js byte-for-byte. Do NOT
// loosen it to match unpaired / dangling <invoke> (truncated turns) — that
// trades the 0-false-positive boundary for marginal recall.
const Anchor = `(?:^|\n)[ \t]*(?:call|<function_calls>)[ \t]*\n[ \t]*<invoke name="`

var re = regexp.MustCompile(Anchor)

// Detect reports whether text contains a leaked tool-call block: a paired
// </invoke> AND the own-line `call` / `<function_calls>` anchor. Both are
// required — the closing-tag check is the cheap fast-path and the anchor is
// the false-positive guard.
func Detect(text string) bool {
	if !strings.Contains(text, "</invoke>") {
		return false
	}
	return re.MatchString(text)
}

// Strip splits a leaked assistant body into the real prose that precedes the
// leaked block and the leaked block itself. It returns (prose, leaked, true)
// on a hit and ("", "", false) when no leak is present.
//
// The leaked region runs from the `call` / `<function_calls>` marker line
// through the LAST </invoke> (plus an optional trailing </function_calls>), so
// multiple chained <invoke> blocks under one marker collapse into one region.
// This mirrors stripLeakedToolCalls in dashboard.js and is used to hand a
// no-fold channel (feishu / weixin) a clean body when recovery cannot complete.
func Strip(text string) (prose, leaked string, found bool) {
	if !strings.Contains(text, "</invoke>") {
		return "", "", false
	}
	loc := re.FindStringIndex(text)
	if loc == nil {
		return "", "", false
	}
	// loc[0] points at the char before the marker line when the alternation
	// matched a leading \n; step over it so the marker stays in `leaked`.
	start := loc[0]
	if start < len(text) && text[start] == '\n' {
		start++
	}
	end := strings.LastIndex(text, "</invoke>") + len("</invoke>")
	if tail := text[end:]; len(tail) > 0 {
		if m := regexp.MustCompile(`^\s*</function_calls>`).FindString(tail); m != "" {
			end += len(m)
		}
	}
	prose = strings.TrimRight(text[:start], " \t\r\n")
	return prose, text[start:end], true
}
