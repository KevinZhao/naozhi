package textutil

import "testing"

func TestFirstLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"all_whitespace", "   \n\t\n", ""},
		{"single_line", "hello", "hello"},
		{"trim_outer", "  hello  ", "hello"},
		{"first_non_empty", "hello\nworld", "hello"},
		{"skip_one_blank", "\nhello\nworld", "hello"},
		{"skip_multiple_blanks", "\n\n\t\nhello\nworld", "hello"},
		{"all_blanks_then_text", "   \n\t\n\nfinally", "finally"},
		{"crlf_treated_as_text", "first\r\nsecond", "first"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FirstLine(tc.in)
			if got != tc.want {
				t.Errorf("FirstLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFirstLineLiteral(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"empty_first_preserved", "\nrest", ""},
		{"single_line", "only", "only"},
		{"first_with_text", "first\nsecond", "first"},
		{"leading_whitespace_preserved", "  spaced  \nrest", "  spaced  "},
		{"crlf_keeps_carriage_return", "first\r\nsecond", "first\r"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FirstLineLiteral(tc.in)
			if got != tc.want {
				t.Errorf("FirstLineLiteral(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
