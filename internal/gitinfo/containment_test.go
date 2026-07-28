package gitinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These cover the findings from the pre-PR review round: the ancestor walk and
// the `.git` gitdir pointer both used to reach outside the caller's declared
// root, and three layouts were misidentified because "is there a `worktrees`
// path component" was used as the worktree test instead of the `commondir`
// marker git actually writes.

// TestDetect_WalkUpStopsAtRoot: validateWorkspace proves only that the
// WORKSPACE is inside allowedRoot. Before the bound, Detect kept walking up and
// reported a parent repo's absolute path and current branch — e.g. an operator
// with allowed_root=<repo>/docs would still see <repo> and its branch.
func TestDetect_WalkUpStopsAtRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	// Repo planted ABOVE the allowed root.
	writeGitDir(t, base, "ref: refs/heads/secret-embargoed\n")
	allowed := filepath.Join(base, "allowed")
	ws := filepath.Join(allowed, "proj", "sub")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	st, ok := Detect(ws, allowed)
	if ok {
		t.Errorf("ok=true: leaked root=%q branch=%q from above allowedRoot", st.Root, st.Branch)
	}

	// Sanity: unbounded, the same call DOES find it — proving the fixture is
	// wired correctly and the bound is what rejects it, not a broken layout.
	if st, ok := Detect(ws, ""); !ok || st.Branch != "secret-embargoed" {
		t.Fatalf("unbounded control failed (ok=%v branch=%q) — fixture is wrong, not the bound", ok, st.Branch)
	}
}

// TestDetect_RootItselfIsAllowed: the bound is inclusive — a repo AT the
// allowed root must still resolve, otherwise the common single-project
// deployment (allowed_root == the repo) would report no branch at all.
func TestDetect_RootItselfIsAllowed(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGitDir(t, root, "ref: refs/heads/master\n")

	st, ok := Detect(root, root)
	if !ok || st.Branch != "master" {
		t.Errorf("got ok=%v branch=%q, want the repo at allowedRoot to resolve", ok, st.Branch)
	}
}

// TestDetect_SiblingPrefixIsNotContained guards against a naive string-prefix
// bound: "/srv/allowed-evil" must not count as inside "/srv/allowed".
func TestDetect_SiblingPrefixIsNotContained(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	sibling := filepath.Join(base, "allowed-evil")
	for _, d := range []string{allowed, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeGitDir(t, sibling, "ref: refs/heads/evil\n")

	if st, ok := Detect(sibling, allowed); ok {
		t.Errorf("ok=true for a sibling sharing the root's name prefix: branch=%q", st.Branch)
	}
}

// TestDetect_GitdirPointerOutsideRootRefused: files inside a workspace are
// agent-writable by design, so a `.git` FILE is untrusted content. An absolute
// or ../.. pointer used to make Detect read a HEAD anywhere on the host.
func TestDetect_GitdirPointerOutsideRootRefused(t *testing.T) {
	t.Parallel()
	for name, pointer := range map[string]string{"absolute": "abs", "relative": "../../outside"} {
		base := t.TempDir()
		allowed := filepath.Join(base, "allowed")
		outside := filepath.Join(base, "outside")
		ws := filepath.Join(allowed, "ws")
		for _, d := range []string{outside, ws} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(outside, "HEAD"), []byte("ref: refs/heads/leaked\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		target := pointer
		if pointer == "abs" {
			target = outside
		}
		if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: "+target+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		if st, ok := Detect(ws, allowed); ok {
			t.Errorf("%s pointer: ok=true, leaked branch=%q from outside allowedRoot", name, st.Branch)
		}
	}
}

// TestDetect_MainTreeInDirNamedWorktrees: this repo keeps its worktrees under
// ".claude/worktrees/", so a `git init` in a directory named "worktrees" is a
// plausible accident. Path-shape detection reported it as a linked worktree
// literally named ".git"; the commondir marker is what distinguishes them.
func TestDetect_MainTreeInDirNamedWorktrees(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "holder", "worktrees")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGitDir(t, dir, "ref: refs/heads/main\n")

	st, ok := Detect(dir, "")
	if !ok {
		t.Fatal("ok=false for a main tree in a dir named worktrees")
	}
	if st.Worktree != "" {
		t.Errorf("Worktree = %q, want empty — this is a main tree, not a linked worktree", st.Worktree)
	}
	if st.Repo != "worktrees" {
		t.Errorf("Repo = %q, want worktrees (its own dir name)", st.Repo)
	}
	if st.Branch != "main" {
		t.Errorf("Branch = %q, want main", st.Branch)
	}
}

// TestDetect_WorktreeOfBareRepo: a bare repo has no working tree, so "two
// levels above worktrees" is not a checkout — it used to yield whatever
// directory happened to contain the bare repo. commondir gives "<name>.git",
// whose stem is the repo name.
func TestDetect_WorktreeOfBareRepo(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	bare := filepath.Join(base, "barerepo.git")
	wtGitDir := filepath.Join(bare, "worktrees", "bare-wt")
	wtTree := filepath.Join(base, "bare-wt")
	for _, d := range []string{wtGitDir, wtTree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "HEAD"), []byte("ref: refs/heads/wt-branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCommonDir(t, wtGitDir, "../..") // → <base>/barerepo.git
	if err := os.WriteFile(filepath.Join(wtTree, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, ok := Detect(wtTree, "")
	if !ok {
		t.Fatal("ok=false for a worktree of a bare repo")
	}
	if st.Worktree != "bare-wt" {
		t.Errorf("Worktree = %q, want bare-wt", st.Worktree)
	}
	if st.Repo != "barerepo" {
		t.Errorf("Repo = %q, want barerepo (the .git suffix stripped), not the containing dir", st.Repo)
	}
}

// TestDetect_WorktreeOfSubmodule: the common dir is
// "<super>/.git/modules/<path>", so the grandparent of "worktrees" is
// "modules/<...>" rather than a working tree. The module path tail is the
// meaningful name.
func TestDetect_WorktreeOfSubmodule(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	modDir := filepath.Join(base, "super", ".git", "modules", "libs", "sub")
	wtGitDir := filepath.Join(modDir, "worktrees", "subm-wt")
	wtTree := filepath.Join(base, "subm-wt")
	for _, d := range []string{wtGitDir, wtTree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "HEAD"), []byte("ref: refs/heads/sub-wt-branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCommonDir(t, wtGitDir, "../..") // → .../modules/libs/sub
	if err := os.WriteFile(filepath.Join(wtTree, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, ok := Detect(wtTree, "")
	if !ok {
		t.Fatal("ok=false for a worktree of a submodule")
	}
	if st.Repo != "sub" {
		t.Errorf("Repo = %q, want sub (the module path tail), not the modules dir", st.Repo)
	}
	if st.Worktree != "subm-wt" {
		t.Errorf("Worktree = %q, want subm-wt", st.Worktree)
	}
}

// TestDetect_WorktreesPathWithoutCommondirIsNotAWorktree isolates the marker
// itself: same path shape as a linked worktree, commondir absent.
func TestDetect_WorktreesPathWithoutCommondirIsNotAWorktree(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	main := filepath.Join(base, "repo")
	gitDir := filepath.Join(main, ".git", "worktrees", "ghost")
	wtTree := filepath.Join(base, "ghost")
	for _, d := range []string{gitDir, wtTree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/ghost-branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately NO commondir.
	if err := os.WriteFile(filepath.Join(wtTree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, ok := Detect(wtTree, "")
	if !ok {
		t.Fatal("ok=false — the branch should still be readable")
	}
	if st.Worktree != "" {
		t.Errorf("Worktree = %q, want empty without a commondir marker", st.Worktree)
	}
}

// TestSanitizeName_DropsBidiAndZeroWidth: the chip answers "which checkout am I
// on", so a name that renders as a DIFFERENT name defeats the feature. Mirrors
// the escJs rune class in static/nz_util.js (R202606j-SEC-9).
func TestSanitizeName_DropsBidiAndZeroWidth(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ in, want string }{
		"RLO override":  {"feat/\u202egnp.evil", "feat/gnp.evil"},
		"zero-width sp": {"mas\u200bter", "master"},
		"RLM":           {"main\u200f", "main"},
		"word joiner":   {"ma\u2060in", "main"},
		"bidi isolate":  {"x\u2066y\u2069z", "xyz"},
		"BOM":           {"\ufeffmain", "main"},
		"C1 control":    {"ma\u0085in", "main"},
		"LS":            {"a\u2028b", "ab"},
		"keeps CJK":     {"功能/新分支", "功能/新分支"},
		"keeps emoji":   {"feat/🚀-ship", "feat/🚀-ship"},
	}
	for name, tc := range cases {
		if got := sanitizeName(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeName(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// TestDetect_BidiBranchNameSanitizedEndToEnd pins the same property through the
// real entry point, not just the helper.
func TestDetect_BidiBranchNameSanitizedEndToEnd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGitDir(t, root, "ref: refs/heads/feat/\u202egnp.evil\n")

	st, ok := Detect(root, "")
	if !ok {
		t.Fatal("ok=false")
	}
	for _, bad := range []rune{0x202e, 0x200b, 0xfeff} {
		if strings.ContainsRune(st.Branch, bad) {
			t.Errorf("Branch = %q still carries U+%04X", st.Branch, bad)
		}
	}
	if st.Branch != "feat/gnp.evil" {
		t.Errorf("Branch = %q, want feat/gnp.evil", st.Branch)
	}
}
