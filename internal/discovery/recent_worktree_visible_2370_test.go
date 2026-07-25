package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #2370: a session run inside a Claude-created git worktree
// ("<repo>/.claude/worktrees/<name>") never appeared in the dashboard history
// panel. Claude Code encodes the CWD by substituting every non-alphanumeric
// character with "-", so the "/." of "/.claude" collapses into "--" and the
// project directory is named "…-repo--claude-worktrees-<name>".
//
// Two independent defects hid it:
//  1. RecentSessions skipped any directory whose *encoded* name contained
//     "--", a pre-decode heuristic meant for tool dirs like claude-mem.
//  2. resolveWorkspaceByParts could not decode the name anyway: splitting on
//     "-" and Stat-ing candidates cannot recover the leading dot of
//     ".claude", so the workspace resolved to "" and the unresolvable-
//     workspace layer dropped it.
//
// Verified red against the pre-fix tree: both TestResolveWorkspaceByParts_
// WorktreeDotClaude ("" instead of the path) and TestRecentSessions_
// IncludesWorktreeSession ("got 0 sessions") failed.
//
// These tests pin both halves plus the property that genuinely tool-owned
// hidden dirs stay filtered.

// makeWorktreeWorkspace builds <tmp>/repo/.claude/worktrees/<name> and returns
// it alongside a fresh ~/.claude dir and the encoded project-dir name.
func makeWorktreeWorkspace(t *testing.T, name string) (claudeDir, workspace, encodedDir string) {
	t.Helper()
	claudeDir = makeClaudeDir(t)
	repo := filepath.Join(t.TempDir(), "repo")
	workspace = filepath.Join(repo, ".claude", "worktrees", name)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	encodedDir = ClaudeProjectSlug(workspace)
	dfsPathCache.Delete(encodedDir)
	t.Cleanup(func() { dfsPathCache.Delete(encodedDir) })
	return claudeDir, workspace, encodedDir
}

// TestResolveWorkspaceByParts_WorktreeDotClaude is the unit-level half: the
// encoded name must decode back to the real worktree path. Pre-fix this
// returned "" because tryResolveParts can only rejoin hyphen-split segments
// and never produces the "." of ".claude".
func TestResolveWorkspaceByParts_WorktreeDotClaude(t *testing.T) {
	_, workspace, encodedDir := makeWorktreeWorkspace(t, "dashboard-pw-replay")

	got := resolveWorkspaceByParts(encodedDir)
	if got != workspace {
		t.Errorf("resolveWorkspaceByParts(%q) = %q, want %q", encodedDir, got, workspace)
	}
}

// TestResolveWorkspaceByParts_DirScanCaches pins that the second (ReadDir)
// pass populates the same positive cache as the first, so the expensive walk
// is paid once per encoded name rather than on every 1Hz sidebar poll.
func TestResolveWorkspaceByParts_DirScanCaches(t *testing.T) {
	_, workspace, encodedDir := makeWorktreeWorkspace(t, "cached-wt")

	if got := resolveWorkspaceByParts(encodedDir); got != workspace {
		t.Fatalf("first resolve = %q, want %q", got, workspace)
	}
	v, ok := dfsPathCache.Load(encodedDir)
	if !ok {
		t.Fatalf("dir-scan result was not cached for %q", encodedDir)
	}
	if v.(string) != workspace {
		t.Errorf("cached value = %q, want %q", v, workspace)
	}
	// Remove the directory: a cached positive must still be served, proving
	// the second call did not re-walk the filesystem.
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	if got := resolveWorkspaceByParts(encodedDir); got != workspace {
		t.Errorf("second resolve = %q, want cached %q", got, workspace)
	}
}

// TestRecentSessions_IncludesWorktreeSession is the end-to-end half: the
// session must actually reach the history slice. Pre-fix the "--" guard
// dropped the directory before workspace resolution even ran.
func TestRecentSessions_IncludesWorktreeSession(t *testing.T) {
	claudeDir, workspace, encodedDir := makeWorktreeWorkspace(t, "dashboard-pw-replay")

	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "284b216a-f22f-4149-bd4b-cdee1ce160ee"
	line := `{"type":"user","timestamp":"2026-07-25T08:19:50.155Z","message":{"role":"user","content":"改造 dashboard"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	got := RecentSessions(claudeDir, 10, 365*24*time.Hour, nil, nil)
	for _, s := range got {
		if s.SessionID != sid {
			continue
		}
		if s.Workspace != workspace {
			t.Errorf("Workspace = %q, want %q", s.Workspace, workspace)
		}
		if s.LastPrompt == "" {
			t.Error("LastPrompt is empty; prompt extraction should have run")
		}
		return
	}
	t.Fatalf("worktree session %s missing from history; got %d sessions", sid, len(got))
}

// TestRecentSessions_StillSkipsToolHiddenDirs guards the other direction: the
// narrowed filter must keep hiding genuinely tool-owned hidden workspaces.
// Without this, widening the "--" rule would leak claude-mem observer prompt
// fragments into the user-facing panel — the reason the guard existed.
func TestRecentSessions_StillSkipsToolHiddenDirs(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	base := t.TempDir()

	cases := []struct {
		name string
		rel  string
	}{
		// A tool's own dot-dir.
		{"claude-mem observer", filepath.Join(".claude-mem", "observer")},
		// A dot-dir that is not the worktrees path.
		{"dot cache dir", filepath.Join(".cache", "someproject")},
		// ".claude" but NOT under worktrees/.
		{"claude internal dir", filepath.Join(".claude", "statsig")},
		// A worktree nested inside a tool dir must not be whitelisted by
		// association with the ".claude/worktrees/" fragment.
		{"worktree under tool dir", filepath.Join(".cache", "x", ".claude", "worktrees", "y")},
		// A dot-component *inside* the worktree name is still hidden.
		{"dot inside worktree name", filepath.Join(".claude", "worktrees", "w", ".hidden")},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := filepath.Join(base, tc.rel)
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			encodedDir := ClaudeProjectSlug(workspace)
			dfsPathCache.Delete(encodedDir)
			t.Cleanup(func() { dfsPathCache.Delete(encodedDir) })

			projDir := filepath.Join(claudeDir, "projects", encodedDir)
			if err := os.MkdirAll(projDir, 0o755); err != nil {
				t.Fatal(err)
			}
			sid := "22222222-0002-0002-0002-00000000000" + string(rune('0'+i))
			if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("data\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			dirFilesCache.Delete(projDir)
			t.Cleanup(func() { dirFilesCache.Delete(projDir) })

			for _, s := range RecentSessions(claudeDir, 50, 365*24*time.Hour, nil, nil) {
				if s.SessionID == sid {
					t.Errorf("tool-owned hidden workspace %q leaked into history: %+v", workspace, s)
				}
			}
		})
	}
}

// TestIsHiddenToolWorkspace covers the decision table directly so the
// whitelist logic is pinned independently of the filesystem fixtures above.
func TestIsHiddenToolWorkspace(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/home/u/workspace/polyquant", false},
		{"/home/u/workspace/polyquant/.claude/worktrees/dashboard-pw-replay", false},
		{"/home/u/workspace/naozhi/.claude/worktrees/fix-alb-health-429", false},
		// Hidden: not the worktrees path.
		{"/home/u/.claude-mem/observer", true},
		{"/home/u/workspace/x/.claude/statsig", true},
		{"/home/u/.cache/x", true},
		// Hidden: worktrees fragment present but a dot component precedes it.
		{"/home/u/.cache/x/.claude/worktrees/y", true},
		// Hidden: dot component after the worktree name.
		{"/home/u/w/.claude/worktrees/y/.git-stuff", true},
		// Hidden: the worktree name itself is dot-prefixed.
		{"/home/u/w/.claude/worktrees/.secret", true},
		// Hidden: the container directory itself is not a worktree. Both the
		// trailing-separator and bare forms must be rejected — the trailing
		// form previously slipped through as visible because the slice
		// arithmetic double-counted the marker's own separator.
		{"/home/u/w/.claude/worktrees/", true},
		{"/home/u/w/.claude/worktrees", true},
		{"/.claude/worktrees/", true},
		// Visible: no component is dot-PREFIXED here ("my.claude" merely
		// contains a dot), so nothing is hidden in the first place and the
		// worktrees marker never comes into play.
		{"/home/u/my.claude/worktrees/x", false},
		// Relative paths: workspace is NOT guaranteed absolute, because
		// resolveWorkspaceWithIndex passes sessions-index.json's originalPath
		// through verbatim. A leading dot component must still count as
		// hidden, or a relative tool dir bypasses the filter entirely.
		{".relhidden-tool/observer", true},
		{".claude/statsig", true},
		{".cache", true},
		// …and a relative worktree path must still be visible.
		{".claude/worktrees/x", false},
		// Relative non-hidden paths stay visible.
		{"workspace/polyquant", false},
		// A path with no separator-dot at all is never hidden, even if a
		// component merely contains a dot.
		{"/home/u/workspace/site.com", false},
	}
	for _, tc := range cases {
		if got := isHiddenToolWorkspace(tc.in); got != tc.want {
			t.Errorf("isHiddenToolWorkspace(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestRecentSessions_RelativeHiddenWorkspaceStillSkipped is the end-to-end
// guard for a regression this fix introduced and then closed: moving the
// hidden-path check from the encoded directory name to the decoded workspace
// made it dependent on the workspace being absolute — which it is not.
// resolveWorkspaceWithIndex returns sessions-index.json's originalPath
// verbatim once os.Stat confirms it is a directory, and that field is file
// content, so a relative ".tool/observer" resolving against the process CWD
// reached the history panel with the tool's prompts attached.
func TestRecentSessions_RelativeHiddenWorkspaceStillSkipped(t *testing.T) {
	claudeDir := makeClaudeDir(t)

	// A relative hidden directory under the process CWD, which is what
	// os.Stat resolves originalPath against.
	const rel = ".relhidden-tool-2370/observer"
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(".relhidden-tool-2370") })

	// The encoded name is deliberately unresolvable so the index's
	// originalPath is the only thing that can supply a workspace.
	projDir := filepath.Join(claudeDir, "projects", "-nonresolvable-relhidden-2370")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "33333333-0003-0003-0003-000000000003"
	line := `{"type":"user","message":{"role":"user","content":"tool internal prompt"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSessionsIndex(t, projDir, sessionsIndex{
		OriginalPath: rel,
		Entries:      []sessionsIndexEntry{{SessionID: sid, Summary: "tool internal"}},
	})
	dirFilesCache.Delete(projDir)
	t.Cleanup(func() { dirFilesCache.Delete(projDir) })

	for _, s := range RecentSessions(claudeDir, 50, 365*24*time.Hour, nil, nil) {
		if s.SessionID == sid {
			t.Errorf("relative hidden workspace leaked into history: ws=%q prompt=%q",
				s.Workspace, s.LastPrompt)
		}
	}
}

// TestResolveByDirScan_AmbiguousPrefersDot pins the tie-break when several
// real directories share one encoded name (".claude", "-claude" and "_claude"
// all encode to "-claude"). Whichever wins must re-encode back to the input —
// that invariant is what stops the history panel from attributing a session to
// an unrelated workspace — and the dot form should win because that is the
// layout Claude itself creates.
func TestResolveByDirScan_AmbiguousPrefersDot(t *testing.T) {
	base := t.TempDir()
	// Only the dot variant plus an underscore sibling: a "-claude" directory
	// would be resolvable by pass 1 and never reach resolveByDirScan.
	for _, variant := range []string{".claude", "_claude"} {
		if err := os.MkdirAll(filepath.Join(base, variant, "worktrees", "x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := filepath.Join(base, ".claude", "worktrees", "x")
	encoded := ClaudeProjectSlug(want)
	dfsPathCache.Delete(encoded)
	t.Cleanup(func() { dfsPathCache.Delete(encoded) })

	got := resolveWorkspaceByParts(encoded)
	if got != want {
		t.Errorf("resolveWorkspaceByParts(%q) = %q, want the dot variant %q", encoded, got, want)
	}
	// The invariant, independent of which variant won.
	if got != "" && ClaudeProjectSlug(got) != encoded {
		t.Errorf("resolved %q re-encodes to %q, want %q", got, ClaudeProjectSlug(got), encoded)
	}
}

// TestDotPreferredOrder covers the reordering helper directly, including the
// no-allocation short-circuits (no dot entries / dot entries already first).
func TestDotPreferredOrder(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", ".hidden", ".zz"} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := dotPreferredOrder(entries)
	if len(got) != len(entries) {
		t.Fatalf("length changed: %d -> %d", len(entries), len(got))
	}
	// os.ReadDir sorts lexically, so dotfiles already lead here and the slice
	// must be returned untouched.
	if &got[0] != &entries[0] {
		t.Error("dot-entries-first input should be returned without copying")
	}

	// Mixed input where a non-dot precedes a dot: reordering must hoist the
	// dot entries while preserving relative order inside each group.
	mixed := []os.DirEntry{entries[2], entries[3], entries[0], entries[1]} // a, b, .hidden, .zz
	reordered := dotPreferredOrder(mixed)
	var names []string
	for _, e := range reordered {
		names = append(names, e.Name())
	}
	want := []string{".hidden", ".zz", "a", "b"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("order = %v, want %v", names, want)
			break
		}
	}
}

// TestClaudeProjectSlug_MatchesCLIForWorktree pins the encoding against a
// real observation from claude CLI 2.1.219: running a session in
// "/home/ec2-user/workspace/polyquant/.claude/worktrees/dashboard-pw-replay"
// produced exactly this project directory. The pre-fix '/'-only substitution
// produced "…-polyquant-.claude-…" instead, so every O(1) JSONL lookup missed
// and silently degraded to a full projects/ scan.
func TestClaudeProjectSlug_MatchesCLIForWorktree(t *testing.T) {
	const cwd = "/home/ec2-user/workspace/polyquant/.claude/worktrees/dashboard-pw-replay"
	const want = "-home-ec2-user-workspace-polyquant--claude-worktrees-dashboard-pw-replay"
	if got := ClaudeProjectSlug(cwd); got != want {
		t.Errorf("ClaudeProjectSlug(%q) =\n  %q\nwant\n  %q", cwd, got, want)
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
	got := ClaudeProjectSlug(long)
	if len(got) <= claudeSlugMaxLen {
		t.Fatalf("slug len = %d, want > %d (cap must append a hash suffix)", len(got), claudeSlugMaxLen)
	}
	prefix, suffix := got[:claudeSlugMaxLen], got[claudeSlugMaxLen:]
	if want := substituteNonAlnum(long)[:claudeSlugMaxLen]; prefix != want {
		t.Errorf("truncated prefix = %q, want %q", prefix, want)
	}
	if suffix[0] != '-' || len(suffix) < 2 {
		t.Errorf("suffix = %q, want \"-<base36 hash>\"", suffix)
	}

	// Two paths sharing the truncation prefix must not collide.
	other := long + "/different"
	if a, b := ClaudeProjectSlug(long), ClaudeProjectSlug(other); a == b {
		t.Errorf("distinct deep paths collided onto %q", a)
	}

	// Exactly at the cap: no suffix. One past it: truncate + suffix. Both
	// sides of the boundary were confirmed against the real CLI (a 201-char
	// CWD produced a 207-char directory name).
	at := "/" + strings.Repeat("a", claudeSlugMaxLen-1)
	if got := ClaudeProjectSlug(at); len(got) != claudeSlugMaxLen || strings.Contains(got[1:], "-") {
		t.Errorf("at cap: ClaudeProjectSlug len = %d, want exactly %d with no suffix", len(got), claudeSlugMaxLen)
	}
	if got := ClaudeProjectSlug(at + "a"); len(got) <= claudeSlugMaxLen {
		t.Errorf("one past cap: len = %d, want > %d", len(got), claudeSlugMaxLen)
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
	got := ClaudeProjectSlug(cwd)
	if got != want {
		t.Errorf("ClaudeProjectSlug(201-char cwd) len=%d, want len=%d\n got=%q\nwant=%q",
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
	if got := ClaudeProjectSlug(cwd); got != want {
		t.Errorf("ClaudeProjectSlug(deep path) =\n  %q (len %d)\nwant\n  %q (len %d)",
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
		if got := ClaudeProjectSlug(tc.in); got != tc.want {
			t.Errorf("ClaudeProjectSlug(%q) = %q (len %d), want %q (len %d)",
				tc.in, got, len(got), tc.want, len(tc.want))
		}
	}
}

// TestSubstituteNonAlnum_InvalidUTF8 guards totality: an arbitrary byte
// sequence (a path from a filesystem with a different encoding) must still
// produce a usable slug rather than panicking or dropping bytes. Each invalid
// byte decodes to RuneError with size 1 and contributes exactly one '-'.
func TestSubstituteNonAlnum_InvalidUTF8(t *testing.T) {
	got := substituteNonAlnum("a\xff\xfeb")
	if want := "a--b"; got != want {
		t.Errorf("substituteNonAlnum(invalid utf-8) = %q, want %q", got, want)
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
		if got := claudeSlugHash(tc.in); got != tc.want {
			t.Errorf("claudeSlugHash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
