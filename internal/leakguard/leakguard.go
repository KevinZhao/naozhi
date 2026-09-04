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
// Nothing executes, the CLI emits a normal end_turn, and the turn stalls.
// Anchor is kept byte-for-byte in lockstep with LEAKED_TOOLCALL_RE in
// internal/server/static/dashboard.js (asserted by a server test).
package leakguard

import (
	"regexp"
	"strings"
)

// Anchor mirrors LEAKED_TOOLCALL_RE in dashboard.js byte-for-byte. It is
// deliberately strict (own-line `call` / `<function_calls>` marker right before
// `<invoke name="`); do not loosen it to match dangling <invoke>.
const Anchor = `(?:^|\n)[ \t]*(?:call|<function_calls>)[ \t]*\n[ \t]*<invoke name="`

var re = regexp.MustCompile(Anchor)

// Detect reports whether text contains a leaked tool-call block: the anchor
// followed LATER by a paired </invoke>. A stray </invoke> before the anchor
// must not count, which also keeps Detect and Strip consistent (#2355).
func Detect(text string) bool {
	loc := re.FindStringIndex(text)
	if loc == nil {
		return false
	}
	return strings.Contains(text[loc[0]:], "</invoke>")
}

// Strip splits a leaked assistant body into the prose before the leaked block
// and the block itself (marker line through the LAST </invoke>, plus an
// optional trailing </function_calls>). Returns ("", "", false) when no leak
// is present. Mirrors stripLeakedToolCalls in dashboard.js.
func Strip(text string) (prose, leaked string, found bool) {
	loc := re.FindStringIndex(text)
	if loc == nil {
		return "", "", false
	}
	// Step over a leading \n matched by the alternation so the marker stays in leaked.
	start := loc[0]
	if start < len(text) && text[start] == '\n' {
		start++
	}
	end := strings.LastIndex(text, "</invoke>") + len("</invoke>")
	// Strip is callable without Detect: a stray </invoke> before the anchor puts
	// LastIndex before start; bail out rather than panic on the slice (#2355).
	if end <= start {
		return "", "", false
	}
	if tail := text[end:]; len(tail) > 0 {
		if m := regexp.MustCompile(`^\s*</function_calls>`).FindString(tail); m != "" {
			end += len(m)
		}
	}
	prose = strings.TrimRight(text[:start], " \t\r\n")
	return prose, text[start:end], true
}
