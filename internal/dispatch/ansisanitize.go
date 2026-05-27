package dispatch

import "regexp"

// ansiEscRe scrubs ANSI / VT-style escape sequences from strings that flow
// from tool_use / thinking events through the IM status banner. The IM
// clients (Feishu / Slack / Discord) render these as garbled mojibake or,
// worse, partial-state terminal commands when surfaced inside a code-block
// preview. We therefore strip them at the dispatch boundary before any
// truncate / append / Reply call so the bytes never leave the gateway.
//
// The alternation covers, in order:
//
//   - CSI: ESC [ params final            (`\x1b[31m`, `\x1b[2J`, …)
//   - OSC: ESC ] payload (BEL | ST)      (`\x1b]8;;url\x07`, hyperlinks)
//   - DCS: ESC P payload ST              (`\x1bPq…\x1b\\`, sixel / Kitty)
//   - SOS: ESC X payload ST              (rarely used, but spec-defined)
//   - PM:  ESC ^ payload ST              (privacy message, status line)
//   - APC: ESC _ payload ST              (`\x1b_Gpayload\x1b\\`, Kitty img)
//   - ESC + single intermediate/final    (`\x1b=`, `\x1b>`, `\x1b(B`, …)
//
// Issue #836 / R238-SEC-13: the previous server-side regex in
// dashboard_cron_transcript.go covered CSI + OSC only (#788). Tool output
// emitted via terminal hyperlinks was already stripped there for the
// transcript viewer, but the *dispatch* path that pushes status banners
// to IM had no scrubbing at all, so tool inputs containing escape bytes
// (a Bash command argument with embedded `\x1b]…` would leak straight to
// chat) were rendered verbatim. This sanitizer closes that gap and also
// covers DCS / SOS / PM / APC which the server regex still misses.
//
// Note: BEL (`\x07`) is allowed as an OSC terminator per xterm; ST in
// our coverage is the 7-bit canonical `ESC \` two-byte form. We do not
// match the bare C1 byte 0x9c (single-byte ST) or 0x9b (single-byte CSI)
// because both overlap UTF-8 continuation bytes and would corrupt
// non-ASCII text — CLI tools emit the 7-bit ESC-prefixed forms in
// practice (xterm always emits 7-bit when not in 8-bit mode, and Go's
// stdout pipes default to 7-bit).
//
// The regex is RE2-safe (no backreferences, no nested quantifiers on the
// same anchor) so the ReDoS exposure is bounded by input length.
var ansiEscRe = regexp.MustCompile(
	`\x1b\[[0-9;?]*[ -/]*[@-~]` + // CSI
		`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` + // OSC
		`|\x1bP[^\x1b]*\x1b\\` + // DCS
		`|\x1bX[^\x1b]*\x1b\\` + // SOS
		`|\x1b\^[^\x1b]*\x1b\\` + // PM
		`|\x1b_[^\x1b]*\x1b\\` + // APC
		`|\x1b[ -/]*[0-9A-Za-z=>]`, // ESC + intermediate(s) + final (`ESC =`, `ESC (B`, …)
)

// stripANSI returns s with all matched escape sequences removed. Returns
// the input unchanged when no ESC byte is present, avoiding the regex
// engine altogether on the hot path (most tool_use bytes are ASCII).
func stripANSI(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			return ansiEscRe.ReplaceAllString(s, "")
		}
	}
	return s
}
