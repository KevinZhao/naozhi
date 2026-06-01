package memory

import (
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// sanitizeWireText strips control / bidi runes from a memory field before it
// reaches the dashboard JSON wire. Memory files are written by the Claude CLI
// and can absorb attacker-influenced content from workspace files, so a
// memory body (or Name/Description) could carry bidi overrides or raw C0
// control bytes (incl. 0x1B ESC) that corrupt visual ordering or trigger ANSI
// escape interpretation when copy-pasted out of the dashboard. [R103901-SEC-4]
//
// Mirrors internal/dashboard/cron/transcript.go sanitizeWireText: drop every
// C0 control rune (< 0x20) except \t / \n / \r, plus the C1 / bidi / LS / PS
// runes flagged by osutil.IsLogInjectionRune. Preserving \t/\n/\r keeps
// multi-line memory bodies rendering correctly in the dashboard's text sink.
//
// Fast path: a string that is already pure ASCII-printable (with the three
// preserved whitespace runes) is returned unchanged so the common case pays
// only one scan and no allocation.
func sanitizeWireText(s string) string {
	if s == "" {
		return s
	}
	dirty := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 0x80 {
			dirty = true
			break
		}
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			dirty = true
			break
		}
	}
	if !dirty {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1 // drop C0 control (incl. 0x1B ESC) except \t / \n / \r
		}
		if osutil.IsLogInjectionRune(r) {
			return -1 // drop C1 / bidi / LS / PS
		}
		return r
	}, s)
}
