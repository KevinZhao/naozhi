package server

import (
	"strings"
	"testing"
)

// TestAnsiEscRe_StripsOSCHyperlinks pins R243-SEC-6 (#788): the ANSI scrub
// regex must remove OSC (operating-system command) sequences in addition to
// CSI sequences, because terminal hyperlinks emit OSC 8 with either the
// BEL (\x07) or ST (\x1b\\) terminator. Tools like `gh`, `ls --hyperlink`,
// language servers, and pagers commonly produce these inside tool_result
// output; leaving them in the JSONL transcript spillover into the dashboard
// <pre> as literal escape soup, hurting forensic readability and (for the
// ST form) potentially confusing terminal-style log shippers downstream.
func TestAnsiEscRe_StripsOSCHyperlinks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "OSC 8 BEL-terminated hyperlink",
			in:   "see \x1b]8;;https://example.com/\x07docs\x1b]8;;\x07 here",
			want: "see docs here",
		},
		{
			name: "OSC 8 ST-terminated hyperlink",
			in:   "open \x1b]8;;file:///tmp/log\x1b\\link\x1b]8;;\x1b\\!",
			want: "open link!",
		},
		{
			name: "CSI still stripped (regression cover)",
			in:   "\x1b[31mERR\x1b[0m: bad",
			want: "ERR: bad",
		},
		{
			name: "Mixed CSI and OSC",
			in:   "\x1b[33mwarn\x1b[0m \x1b]8;;https://x/\x07see\x1b]8;;\x07!",
			want: "warn see!",
		},
		{
			name: "Plain text untouched",
			in:   "hello world",
			want: "hello world",
		},
	}

	for _, tc := range cases {
		got := ansiEscRe.ReplaceAllString(tc.in, "")
		if got != tc.want {
			t.Errorf("%s:\n  in   = %q\n  got  = %q\n  want = %q", tc.name, tc.in, got, tc.want)
		}
		// Drop-dead invariant: no ESC byte survives in any sanitised output.
		if strings.ContainsRune(got, 0x1b) {
			t.Errorf("%s: ESC byte (0x1b) survived sanitisation: %q", tc.name, got)
		}
	}
}
