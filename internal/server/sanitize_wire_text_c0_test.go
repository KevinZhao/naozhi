package server

import (
	"strings"
	"testing"
)

// TestSanitizeWireText_DropsC0Control_ExceptTabNewlineCR pins #1331
// (R20260527122801-SEC-7): sanitizeWireText must drop C0 control bytes
// (< 0x20) other than \t / \n / \r so 0x1B ESC and friends do not reach
// the dashboard JSON wire and get interpreted as ANSI escapes when an
// operator copy-pastes transcript JSON into a terminal viewer.
func TestSanitizeWireText_DropsC0Control_ExceptTabNewlineCR(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"esc dropped", "hi\x1b[31mred\x1b[0m", "hi[31mred[0m"},
		{"bell dropped", "alert\x07now", "alertnow"},
		{"vertical tab dropped", "a\x0bb", "ab"},
		{"form feed dropped", "a\x0cb", "ab"},
		{"null dropped", "a\x00b", "ab"},
		{"tab preserved", "col1\tcol2", "col1\tcol2"},
		{"newline preserved", "line1\nline2", "line1\nline2"},
		{"cr preserved", "row1\rrow2", "row1\rrow2"},
		{"pure ascii unchanged", "hello world", "hello world"},
		{"empty unchanged", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeWireText(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeWireText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Make sure no C0 control byte other than the whitelist sneaks through.
			for i := 0; i < len(got); i++ {
				b := got[i]
				if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
					t.Fatalf("sanitizeWireText output retained C0 byte 0x%02X: %q", b, got)
				}
			}
		})
	}
}

// TestSanitizeWireText_StillDropsBidi keeps the original IsLogInjectionRune
// guarantee intact alongside the new C0 filtering.
func TestSanitizeWireText_StillDropsBidi(t *testing.T) {
	// U+202E RIGHT-TO-LEFT OVERRIDE
	in := "safe‮evil"
	got := sanitizeWireText(in)
	if strings.ContainsRune(got, '‮') {
		t.Fatalf("sanitizeWireText kept bidi override: %q", got)
	}
}
