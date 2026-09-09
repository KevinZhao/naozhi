package claudefs

// Parity tests for the Claude CLI's project-directory encoding, moved here with
// the implementation (#2643). They pin Go against observations of a real CLI and
// against a node one-liner — never against this implementation — so a rewrite
// that is self-consistently wrong still fails.

import (
	"strings"
	"testing"
)

// TestClaudeProjectSlug_MatchesCLIForWorktree pins the encoding against a
// real observation from claude CLI 2.1.219: running a session in
// "/home/ec2-user/workspace/polyquant/.claude/worktrees/dashboard-pw-replay"
// produced exactly this project directory. The pre-fix '/'-only substitution
// produced "…-polyquant-.claude-…" instead, so every O(1) JSONL lookup missed
// and silently degraded to a full projects/ scan.
func TestClaudeProjectSlug_MatchesCLIForWorktree(t *testing.T) {
	const cwd = "/home/ec2-user/workspace/polyquant/.claude/worktrees/dashboard-pw-replay"
	const want = "-home-ec2-user-workspace-polyquant--claude-worktrees-dashboard-pw-replay"
	if got := ProjectSlug(cwd); got != want {
		t.Errorf("ProjectSlug(%q) =\n  %q\nwant\n  %q", cwd, got, want)
	}
}

// TestClaudeProjectSlug_LengthCapHash pins the CLI's overflow behaviour: past
// 200 encoded bytes the name is truncated to exactly 200 and a base36 hash of
// the ORIGINAL path is appended after a "-". Without this, two deep sibling
// worktrees sharing a 200-byte prefix would collide onto one directory.
func TestClaudeProjectSlug_LengthCapHash(t *testing.T) {
	// 30 segments × 8 bytes = 240 encoded bytes, comfortably over the cap.
	long := ""
	for i := 0; i < 30; i++ {
		long += "/segment" // 8 bytes each
	}
	got := ProjectSlug(long)
	if len(got) <= slugMaxLen {
		t.Fatalf("slug len = %d, want > %d (cap must append a hash suffix)", len(got), slugMaxLen)
	}
	prefix, suffix := got[:slugMaxLen], got[slugMaxLen:]
	if want := EncodeSegment(long)[:slugMaxLen]; prefix != want {
		t.Errorf("truncated prefix = %q, want %q", prefix, want)
	}
	if suffix[0] != '-' || len(suffix) < 2 {
		t.Errorf("suffix = %q, want \"-<base36 hash>\"", suffix)
	}

	// Two paths sharing the truncation prefix must not collide.
	other := long + "/different"
	if a, b := ProjectSlug(long), ProjectSlug(other); a == b {
		t.Errorf("distinct deep paths collided onto %q", a)
	}

	// Exactly at the cap: no suffix. One past it: truncate + suffix. Both
	// sides of the boundary were confirmed against the real CLI (a 201-char
	// CWD produced a 207-char directory name).
	at := "/" + strings.Repeat("a", slugMaxLen-1)
	if got := ProjectSlug(at); len(got) != slugMaxLen || strings.Contains(got[1:], "-") {
		t.Errorf("at cap: ClaudeProjectSlug len = %d, want exactly %d with no suffix", len(got), slugMaxLen)
	}
	if got := ProjectSlug(at + "a"); len(got) <= slugMaxLen {
		t.Errorf("one past cap: len = %d, want > %d", len(got), slugMaxLen)
	}
}

// TestClaudeProjectSlug_CapBoundaryLiveCLI is the captured fixture for the
// truncate+hash boundary: a 201-character CWD created on disk, a session run
// in it, and the resulting directory name recorded verbatim from claude CLI
// 2.1.219. Pairs with TestClaudeProjectSlug_LiveCLIParity (240 chars) so both
// a just-over-the-cap and a well-over-the-cap path are pinned.
func TestClaudeProjectSlug_CapBoundaryLiveCLI(t *testing.T) {
	cwd := "/tmp/sl/" + strings.Repeat("a", 193) // 201 chars
	want := "-tmp-sl-" + strings.Repeat("a", 192) + "-eo33o2"
	got := ProjectSlug(cwd)
	if got != want {
		t.Errorf("ProjectSlug(201-char cwd) len=%d, want len=%d\n got=%q\nwant=%q",
			len(got), len(want), got, want)
	}
}

// TestClaudeProjectSlug_LiveCLIParity is a byte-for-byte fixture captured from
// claude CLI 2.1.219: a 40-segment CWD was created on disk, a session run in
// it, and the resulting ~/.claude/projects/ entry recorded verbatim below.
// It exercises both the substitution and the >200-byte truncate+hash branch
// (note the "--" at the cut point, where truncation lands right after
// "seg30" and the "-" joining the hash follows).
func TestClaudeProjectSlug_LiveCLIParity(t *testing.T) {
	const cwd = "/tmp/slugtest/seg00/seg01/seg02/seg03/seg04/seg05/seg06/seg07/seg08/seg09/" +
		"seg10/seg11/seg12/seg13/seg14/seg15/seg16/seg17/seg18/seg19/" +
		"seg20/seg21/seg22/seg23/seg24/seg25/seg26/seg27/seg28/seg29/" +
		"seg30/seg31/seg32/seg33/seg34/seg35/seg36/seg37/seg38/seg39"
	const want = "-tmp-slugtest-seg00-seg01-seg02-seg03-seg04-seg05-seg06-seg07-seg08-seg09" +
		"-seg10-seg11-seg12-seg13-seg14-seg15-seg16-seg17-seg18-seg19" +
		"-seg20-seg21-seg22-seg23-seg24-seg25-seg26-seg27-seg28-seg29" +
		"-seg30--7lct9w"
	if got := ProjectSlug(cwd); got != want {
		t.Errorf("ProjectSlug(deep path) =\n  %q (len %d)\nwant\n  %q (len %d)",
			got, len(got), want, len(want))
	}
}

// TestClaudeProjectSlug_NonASCIIUTF16Parity pins the substitution unit as one
// UTF-16 code unit, not one byte. Both fixtures were captured by actually
// running claude CLI 2.1.219 in the given directory and reading back the
// created ~/.claude/projects/ entry:
//
//	/tmp/slugtest2/中文目录  ->  -tmp-slugtest2-----     (1 sep + 4 ideographs)
//	/tmp/slugtest3/😀x       ->  -tmp-slugtest3---x      (1 sep + surrogate pair)
//
// A per-byte walk would emit 3 dashes per ideograph and 4 for the emoji,
// yielding a directory name that does not exist on disk — so history lookups
// for any non-ASCII workspace would silently miss.
func TestClaudeProjectSlug_NonASCIIUTF16Parity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/tmp/slugtest2/中文目录", "-tmp-slugtest2-----"},
		{"/tmp/slugtest3/😀x", "-tmp-slugtest3---x"},
		// Mixed: alnum survives, everything else collapses per code unit —
		// 2 separators + 3 ideographs = 5 dashes between "a" and "b".
		{"/a/日本語/b", "-a-----b"},
		// An emoji is one code POINT but two code UNITS, so the same shape
		// with a single emoji yields 4 dashes, not 3.
		{"/a/😀/b", "-a----b"},
	}
	for _, tc := range cases {
		if got := ProjectSlug(tc.in); got != tc.want {
			t.Errorf("ProjectSlug(%q) = %q (len %d), want %q (len %d)",
				tc.in, got, len(got), tc.want, len(tc.want))
		}
	}
}

// TestSubstituteNonAlnum_InvalidUTF8 guards totality: an arbitrary byte
// sequence (a path from a filesystem with a different encoding) must still
// produce a usable slug rather than panicking or dropping bytes. Each invalid
// byte decodes to RuneError with size 1 and contributes exactly one '-'.
func TestSubstituteNonAlnum_InvalidUTF8(t *testing.T) {
	got := EncodeSegment("a\xff\xfeb")
	if want := "a--b"; got != want {
		t.Errorf("EncodeSegment(invalid utf-8) = %q, want %q", got, want)
	}
}

// TestClaudeSlugHash_MatchesJS pins claudeSlugHash against values computed
// with the CLI's own expression, so a future refactor cannot silently drift
// from the JS semantics (int32 wraparound, base36, Math.abs):
//
//	node -e 'let h=0; for (const c of S) h=(h<<5)-h+c.charCodeAt(0)|0;
//	         console.log(Math.abs(h).toString(36))'
func TestClaudeSlugHash_MatchesJS(t *testing.T) {
	// Expected values produced by the node one-liner above, not by this
	// implementation — the point is to pin Go against JS, not against itself.
	cases := []struct{ in, want string }{
		{"a", "2p"},
		{"/home/user", "nm0yb0"},
		{"/home/ec2-user/workspace/polyquant/.claude/worktrees/dashboard-pw-replay", "7f2k5y"},
	}
	for _, tc := range cases {
		if got := slugHash(tc.in); got != tc.want {
			t.Errorf("slugHash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
