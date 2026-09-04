package dispatch

import "regexp"

// ansiEscRe scrubs ANSI / VT-style escape sequences (CSI, OSC, DCS, SOS, PM,
// APC, ESC+final; see the per-alternative notes) from tool_use / thinking
// text before it reaches the IM status banner, where IM clients render them
// as mojibake (#836). ST is matched only as the 7-bit `ESC \` form: the bare
// C1 bytes 0x9c / 0x9b overlap UTF-8 continuation bytes and would corrupt
// non-ASCII text; CLI tools emit 7-bit forms in practice. RE2-safe (no
// backreferences / nested quantifiers), so ReDoS exposure is bounded by
// input length.
var ansiEscRe = regexp.MustCompile(
	`\x1b\[[0-9;?]*[ -/]*[@-~]` + // CSI
		`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` + // OSC
		`|\x1bP[^\x1b]*\x1b\\` + // DCS
		`|\x1bX[^\x1b]*\x1b\\` + // SOS
		`|\x1b\^[^\x1b]*\x1b\\` + // PM
		`|\x1b_[^\x1b]*\x1b\\` + // APC
		`|\x1b[ -/]*[0-9A-Za-z=>]`, // ESC + intermediate(s) + final (`ESC =`, `ESC (B`, …)
)

// stripANSI returns s with all escape sequences removed; skips the regex
// entirely when no ESC byte is present.
func stripANSI(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			return ansiEscRe.ReplaceAllString(s, "")
		}
	}
	return s
}
