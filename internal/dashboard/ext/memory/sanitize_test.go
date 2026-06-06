package memory

import "testing"

// TestSanitizeWireText_R103901_SEC_4 pins R103901-SEC-4: memory field text is
// scrubbed of control / bidi runes before it reaches the dashboard wire, while
// legitimate ASCII / multibyte text and the three preserved whitespace runes
// survive unchanged.
func TestSanitizeWireText_R103901_SEC_4(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain ascii unchanged", "hello world", "hello world"},
		{"preserved whitespace", "a\tb\nc\rd", "a\tb\nc\rd"},
		{"unicode text unchanged", "café 日本語 ✓", "café 日本語 ✓"},
		{"strip esc", "before\x1b[31mred\x1b[0mafter", "before[31mred[0mafter"},
		{"strip nul and bell", "a\x00b\x07c", "abc"},
		{"strip bidi override RLO U+202E", "user‮gnp.exe", "usergnp.exe"},
		// Bidi isolates U+2066 (LRI) and U+2069 (PDI) are stripped; LRM/RLM
		// (U+200E/200F) are intentionally NOT covered — this mirrors the
		// osutil.IsLogInjectionRune policy shared with the cron transcript
		// sanitizer.
		{"strip bidi isolates U+2066/U+2069", "a⁦b⁩c", "abc"},
		{"strip c1 control NEL U+0085", "ab", "ab"},
		{"strip line separator U+2028", "a b", "ab"},
		{"strip paragraph separator U+2029", "a b", "ab"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeWireText(c.in); got != c.want {
				t.Errorf("sanitizeWireText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
