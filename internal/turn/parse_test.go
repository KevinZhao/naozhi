package turn

import "testing"

// TestParse pins the turn commands both entry points share: the command
// token is case-insensitive and whitespace-tolerant, the argument keeps its
// case, and look-alikes are CmdNone.
func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		kind CmdKind
		arg  string
		text string
	}{
		{"/new", CmdReset, "", "/new"},
		{"/clear", CmdReset, "", "/clear"},
		{"/NEW", CmdReset, "", "/new"},
		{"/Clear", CmdReset, "", "/clear"},
		{"  /new  ", CmdReset, "", "/new"},
		{"/new　", CmdReset, "", "/new"},
		{"/New ReviewerBot", CmdReset, "ReviewerBot", "/new ReviewerBot"},
		{"/clear  x ", CmdReset, "x", "/clear  x"},
		{"/newer", CmdNone, "", "/newer"},
		{"/new\tfoo", CmdNone, "", "/new\tfoo"},
		{"/urgent", CmdUrgentUsage, "", "/urgent"},
		{"/URGENT ", CmdUrgentUsage, "", "/urgent"},
		{"/urgent Stop That", CmdUrgent, "Stop That", "/urgent Stop That"},
		{"/Urgent   now", CmdUrgent, "now", "/urgent   now"},
		{"/urgently", CmdNone, "", "/urgently"},
		{"hello /new", CmdNone, "", "hello /new"},
		{"/Help", CmdNone, "", "/help"},
		{"/Cd /Path/To/Dir", CmdNone, "", "/cd /Path/To/Dir"},
		{"", CmdNone, "", ""},
	}
	for _, tc := range cases {
		got := Parse(tc.in)
		if got != (Cmd{Kind: tc.kind, Arg: tc.arg, Text: tc.text}) {
			t.Errorf("Parse(%q) = %+v, want {%v %q %q}", tc.in, got, tc.kind, tc.arg, tc.text)
		}
	}
}

// TestNormalizeCommand lowercases only the leading token of a slash command.
func TestNormalizeCommand(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"/Help", "/help"},
		{"/NEW", "/new"},
		{"/Cd /Path/To/Dir", "/cd /Path/To/Dir"},
		{"/cron add \"Job Name\"", "/cron add \"Job Name\""},
		{"/Pwd　", "/pwd"},
		{"hello World", "hello World"},
	}
	for _, tc := range cases {
		if got := NormalizeCommand(tc.in); got != tc.want {
			t.Errorf("NormalizeCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
