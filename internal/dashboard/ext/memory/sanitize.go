package memory

import (
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// sanitizeWireText strips control / bidi runes from a memory field before it
// reaches the dashboard JSON wire: memory files are CLI-written and can absorb
// attacker-influenced workspace content (bidi overrides, raw C0 incl. ESC).
// Mirrors dashboard/cron/transcript.go sanitizeWireText: drop C0 (< 0x20)
// except \t / \n / \r plus the runes flagged by osutil.IsLogInjectionRune.
// Fast path: an already-clean ASCII string is returned without allocation.
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
