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
//   - DEL (0x7F) and space: not *stripped* (only bytes < 0x20 are), but
//     substituted to "-" like every other non-alphanumeric character.
//     Verified against claude CLI 2.1.219: cwd "/tmp/slugtest/a b_c.d"
//     produces the project dir "-tmp-slugtest-a-b-c-d", and a DEL in the
//     path likewise lands as "-".
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
		// DEL is >= 0x20 so it is not *stripped*, but like every
		// non-alphanumeric byte the CLI substitutes it with "-".
		{"del substituted", "/foo\x7Fbar", "-foo-bar"},
		// Space is a legitimate directory-name character; it survives the
		// control-byte strip and is then substituted, not dropped, so the
		// segment keeps its length (one "-" per space).
		{"space substituted", "/foo bar/baz", "-foo-bar-baz"},
		// Dot and underscore are substituted too — this is what makes a
		// git worktree path encode "/.claude/" as "--claude-" (#2370).
		{"dot and underscore", "/home/u/.claude/a_b", "-home-u--claude-a-b"},
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
	// One alloc is unavoidable: substituteNonAlnum must build a fresh string
	// when at least one substitution happens (every '/' is replaced). What we
	// want to verify is that neither the control-byte filter nor the
	// length-cap branch adds a SECOND allocation on the clean path.
	if allocs > 1 {
		t.Errorf("clean-path ClaudeProjectSlug allocs/run = %.1f, want ≤ 1 (control-byte filter must short-circuit when input is clean)", allocs)
	}
}
