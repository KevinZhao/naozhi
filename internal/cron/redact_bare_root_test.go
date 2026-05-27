package cron

import "testing"

// TestRedactPathsInCronError_BareRoot pins R20260527-COR-10 (#1292): a
// lone '/' at end-of-string OR followed by whitespace/newline carries no
// per-host or per-user information and is intentionally treated as a
// literal byte. Multi-segment paths like /home/u still trigger redaction.
//
// This test exists so a future cleanup of the scanner that "tightens" the
// trigger predicate cannot silently change the documented behaviour of
// the bare-root cases.
func TestRedactPathsInCronError_BareRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"bare slash at EOF", "error: /", "error: /"},
		{"slash before newline", "err: /\nnext", "err: /\nnext"},
		{"slash before space", "matches / glob", "matches / glob"},
		{"slash before tab", "left /\tright", "left /\tright"},
		// Sanity checks: real paths still redact.
		{"posix path before newline", "err: /home/u/x\nnext", "err: <path>\nnext"},
		{"posix path at EOF", "err: /home/u/x", "err: <path>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactPathsInCronError(tc.input)
			if got != tc.want {
				t.Errorf("redactPathsInCronError(%q)\n  got  = %q\n  want = %q", tc.input, got, tc.want)
			}
		})
	}
}
