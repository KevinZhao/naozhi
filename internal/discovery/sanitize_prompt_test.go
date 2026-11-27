package discovery

import (
	"testing"
)

// TestSanitizePromptForTransport: C0 controls but tab, DEL and the runes
// osutil.IsLogInjectionRune flags (C1 NEL, bidi override) become '_'; clean
// text, multi-byte UTF-8 included, comes back unchanged (R20260608133928-PERF-15
// keeps that on the fast path).
func TestSanitizePromptForTransport(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" = unchanged
	}{
		{"empty", "", ""},
		{"pure ASCII", "Hello, world! This is a test prompt.", ""},
		{"pure Chinese", "你好，世界！", ""},
		{"emoji", "Hello 🌍", ""},
		{"accented Latin", "café résumé", ""},
		{"mixed Chinese and ASCII", "I love 脑汁", ""},
		{"tab", "col1\tcol2", ""},
		{"Chinese with tab", "你\t好", ""},
		{"lone tab", "\t", ""},
		{"NUL byte", "foo\x00bar", "foo_bar"},
		{"newline", "foo\nbar", "foo_bar"},
		{"carriage return", "foo\rbar", "foo_bar"},
		{"ESC", "foo\x1bbar", "foo_bar"},
		{"DEL (0x7f)", "foo\x7fbar", "foo_bar"},
		{"Chinese with C0", "你好\x01世界", "你好_世界"},
		{"NEL U+0085", "foo\u0085bar", "foo_bar"},
		{"bidi override U+202E", "foo\u202ebar", "foo_bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.in
			}
			if got := SanitizePromptForTransport(tc.in); got != want {
				t.Errorf("SanitizePromptForTransport(%q) = %q, want %q", tc.in, got, want)
			}
		})
	}
}
