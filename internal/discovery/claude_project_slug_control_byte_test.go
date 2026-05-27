package discovery

import "testing"

// TestClaudeProjectSlug_ControlByteFilter pins R241-SEC-4 (#465):
// hand-edited persisted state (cron_jobs.json, sessions-index.json)
// can carry embedded control bytes in the WorkDir field. Without this
// filter the bytes would survive the slug encoding and end up in the
// resulting filesystem path component, where downstream Stat/Open
// calls produce confusingly-quoted error messages or accidentally
// match an attacker-prepared dir.
//
// Coverage:
//   - clean input: no allocation, identical encoding to legacy contract
//   - tab / newline / carriage return: stripped before encoding
//   - DEL (0x7F): NOT stripped — only bytes < 0x20 are control bytes
//     under POSIX, and the existing scanner contract treats higher
//     bytes (including UTF-8 multi-byte sequences) as opaque.
//   - empty: identity
func TestClaudeProjectSlug_ControlByteFilter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "/home/user/workspace/foo", "-home-user-workspace-foo"},
		{"tab embedded", "/home/user\t/foo", "-home-user-foo"},
		{"newline embedded", "/home/user\n/foo", "-home-user-foo"},
		{"carriage return", "/home/user\r/foo", "-home-user-foo"},
		{"null byte", "/home/u\x00ser/foo", "-home-user-foo"},
		{"all control", "\x00\x01\x02\x03", ""},
		// DEL is >= 0x20, so it survives — this matches the contract
		// "control bytes are < 0x20".
		{"del survives", "/foo\x7Fbar", "-foo\x7Fbar"},
		// Trailing/leading whitespace that's NOT a control byte (>=0x20)
		// survives — Space (0x20) is allowed because it's a legitimate
		// (though atypical) directory name character.
		{"space survives", "/foo bar/baz", "-foo bar-baz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClaudeProjectSlug(tc.in)
			if got != tc.want {
				t.Errorf("ClaudeProjectSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestClaudeProjectSlug_NoAllocOnCleanPath asserts the fast path
// (hasControlByte=false) does not pay for the strip-loop allocation.
// Important because ClaudeProjectSlug runs in dashboard sidebar fetch
// + every cron transcript URL resolution; a hidden alloc per call
// would compound under steady-state load.
func TestClaudeProjectSlug_NoAllocOnCleanPath(t *testing.T) {
	const clean = "/home/ec2-user/workspace/naozhi"
	allocs := testing.AllocsPerRun(100, func() {
		_ = ClaudeProjectSlug(clean)
	})
	// One alloc is unavoidable: strings.ReplaceAll always returns a
	// fresh string when at least one substitution happens (every '/' is
	// replaced). What we want to verify is that the new control-byte
	// filter does NOT add a SECOND allocation on the clean path.
	if allocs > 1 {
		t.Errorf("clean-path ClaudeProjectSlug allocs/run = %.1f, want ≤ 1 (control-byte filter must short-circuit when input is clean)", allocs)
	}
}
