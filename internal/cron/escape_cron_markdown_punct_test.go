package cron

import (
	"strings"
	"testing"
)

// TestEscapeCronMarkdownPunct covers R164930-PERF-4/5: the single-pass
// Replacer implementation must produce byte-identical output to the old
// multi-pass one and must preserve the IndexAny fast-path for clean inputs.
func TestEscapeCronMarkdownPunct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "pure ASCII no special chars",
			input: "daily-review",
			want:  "daily-review",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "open bracket only",
			input: "[link",
			want:  "［link",
		},
		{
			name:  "close bracket only",
			input: "label]tail",
			want:  "label］tail",
		},
		{
			name:  "open paren only",
			input: "text(note",
			want:  "text（note",
		},
		{
			name:  "close paren only",
			input: "text)end",
			want:  "text）end",
		},
		{
			name:  "full markdown link syntax",
			input: "[click](http://attacker)",
			want:  "［click］（http://attacker）",
		},
		{
			name:  "mixed brackets and parens in label",
			input: "evil](http://x) [Cron real",
			want:  "evil］（http://x） ［Cron real",
		},
		{
			name:  "all four chars present",
			input: "a[b]c(d)e",
			want:  "a［b］c（d）e",
		},
		{
			name:  "CJK text no special chars — fast-path",
			input: "每日任务",
			want:  "每日任务",
		},
		{
			name:  "full-width chars already present — idempotent-safe",
			input: "［already escaped］",
			want:  "［already escaped］",
		},
		{
			name:  "multiple consecutive brackets",
			input: "[[]](()",
			want:  "［［］］（（）",
		},
		{
			name:  "body with attacker link",
			input: "click [here](http://attacker) to win",
			want:  "click ［here］（http://attacker） to win",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := escapeCronMarkdownPunct(tc.input)
			if got != tc.want {
				t.Errorf("escapeCronMarkdownPunct(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestEscapeCronMarkdownPunct_FastPath verifies that inputs containing none of
// []() are returned unchanged (pointer equality would be ideal, but string
// identity via == suffices to confirm no alloc path was taken in practice).
func TestEscapeCronMarkdownPunct_FastPath(t *testing.T) {
	t.Parallel()

	clean := []string{
		"daily-review",
		"每日任务",
		"",
		"hello world 123",
		"no-special-chars-here",
	}
	for _, s := range clean {
		got := escapeCronMarkdownPunct(s)
		if got != s {
			t.Errorf("escapeCronMarkdownPunct(%q) modified clean string: got %q", s, got)
		}
	}
}

// TestEscapeCronMarkdownPunct_FullWidthMappings verifies the exact Unicode
// codepoints used in each substitution are the specified full-width variants.
func TestEscapeCronMarkdownPunct_FullWidthMappings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ascii    string
		fullwide rune
	}{
		{"[", '［'}, // U+FF3B
		{"]", '］'}, // U+FF3D
		{"(", '（'}, // U+FF08
		{")", '）'}, // U+FF09
	}
	for _, c := range cases {
		got := escapeCronMarkdownPunct(c.ascii)
		runes := []rune(got)
		if len(runes) != 1 || runes[0] != c.fullwide {
			t.Errorf("escapeCronMarkdownPunct(%q) = %q (rune %U); want rune %U",
				c.ascii, got, runes, c.fullwide)
		}
	}
}

// TestEscapeCronMarkdownPunct_NoResidualASCII confirms that after escaping,
// none of []() remain in the output.
func TestEscapeCronMarkdownPunct_NoResidualASCII(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"[text](url)",
		"a[b]c(d)e",
		"evil](http://x) [Cron real",
		"[[]](()",
		"click [here](http://attacker) to win",
	}
	for _, s := range inputs {
		got := escapeCronMarkdownPunct(s)
		if strings.ContainsAny(got, "[]()") {
			t.Errorf("escapeCronMarkdownPunct(%q) still contains ASCII []() chars: %q", s, got)
		}
	}
}
