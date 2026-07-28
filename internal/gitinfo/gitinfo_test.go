package gitinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeGitDir lays out a minimal main-worktree ".git" directory with the
// given HEAD content.
func writeGitDir(t *testing.T, root, head string) {
	t.Helper()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(head), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
}

// writeCommonDir writes the "commondir" marker git puts in every linked
// worktree's git dir. Fixtures must include it or Detect (correctly) refuses to
// call the layout a worktree — real git always writes it, verified against the
// live worktrees of this repo, which all contain "../..".
func writeCommonDir(t *testing.T, gitDir, rel string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(rel+"\n"), 0o644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
}

func TestDetect_MainWorktreeOnBranch(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGitDir(t, root, "ref: refs/heads/master\n")

	st, ok := Detect(root, "")
	if !ok {
		t.Fatal("Detect returned ok=false for a valid repo")
	}
	if st.Branch != "master" {
		t.Errorf("Branch = %q, want master", st.Branch)
	}
	if st.Detached {
		t.Error("Detached = true, want false on a branch")
	}
	if st.Worktree != "" {
		t.Errorf("Worktree = %q, want empty for the main tree", st.Worktree)
	}
	if st.Repo != "myrepo" {
		t.Errorf("Repo = %q, want myrepo", st.Repo)
	}
	if st.Root != root {
		t.Errorf("Root = %q, want %q", st.Root, root)
	}
	if st.HeadSHA != "" {
		t.Errorf("HeadSHA = %q, want empty on a branch", st.HeadSHA)
	}
}

func TestDetect_SlashedBranchName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGitDir(t, root, "ref: refs/heads/feat/dashboard-git\n")

	st, ok := Detect(root, "")
	if !ok {
		t.Fatal("ok=false")
	}
	if st.Branch != "feat/dashboard-git" {
		t.Errorf("Branch = %q, want feat/dashboard-git", st.Branch)
	}
}

func TestDetect_WalksUpFromSubdirectory(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "repo")
	sub := filepath.Join(root, "internal", "server", "static")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGitDir(t, root, "ref: refs/heads/main\n")

	st, ok := Detect(sub, "")
	if !ok {
		t.Fatal("ok=false for a nested dir inside a repo")
	}
	if st.Root != root {
		t.Errorf("Root = %q, want the working-tree root %q", st.Root, root)
	}
	if st.Branch != "main" {
		t.Errorf("Branch = %q, want main", st.Branch)
	}
}

func TestDetect_DetachedHead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGitDir(t, root, "0123456789abcdef0123456789abcdef01234567\n")

	st, ok := Detect(root, "")
	if !ok {
		t.Fatal("ok=false")
	}
	if !st.Detached {
		t.Error("Detached = false, want true for a raw sha HEAD")
	}
	if st.Branch != "" {
		t.Errorf("Branch = %q, want empty when detached", st.Branch)
	}
	if st.HeadSHA != "0123456" {
		t.Errorf("HeadSHA = %q, want 0123456 (7 hex)", st.HeadSHA)
	}
}

func TestDetect_DetachedHeadSHA256(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sha := strings.Repeat("ab", 32) // 64 hex chars
	writeGitDir(t, root, sha+"\n")

	st, ok := Detect(root, "")
	if !ok {
		t.Fatal("ok=false for a sha256 object id")
	}
	if !st.Detached || st.HeadSHA != "abababa" {
		t.Errorf("got Detached=%v HeadSHA=%q, want true/abababa", st.Detached, st.HeadSHA)
	}
}

func TestDetect_LinkedWorktree(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	mainTree := filepath.Join(base, "naozhi")
	wtTree := filepath.Join(mainTree, ".claude", "worktrees", "feat-x")
	commonGit := filepath.Join(mainTree, ".git")
	wtGitDir := filepath.Join(commonGit, "worktrees", "feat-x")

	for _, d := range []string{wtTree, wtGitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(commonGit, "HEAD"), []byte("ref: refs/heads/master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "HEAD"), []byte("ref: refs/heads/worktree-feat-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCommonDir(t, wtGitDir, "../..")
	// The worktree's ".git" is a file pointing at the common git dir entry.
	if err := os.WriteFile(filepath.Join(wtTree, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, ok := Detect(wtTree, "")
	if !ok {
		t.Fatal("ok=false for a linked worktree")
	}
	if st.Worktree != "feat-x" {
		t.Errorf("Worktree = %q, want feat-x", st.Worktree)
	}
	if st.Branch != "worktree-feat-x" {
		t.Errorf("Branch = %q, want the worktree's own branch", st.Branch)
	}
	if st.Repo != "naozhi" {
		t.Errorf("Repo = %q, want the main tree name naozhi", st.Repo)
	}
	if st.Root != wtTree {
		t.Errorf("Root = %q, want the worktree dir %q", st.Root, wtTree)
	}
}

func TestDetect_LinkedWorktreeRelativePointer(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	mainTree := filepath.Join(base, "repo")
	wtTree := filepath.Join(mainTree, "wt", "a")
	wtGitDir := filepath.Join(mainTree, ".git", "worktrees", "a")
	for _, d := range []string{wtTree, wtGitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "HEAD"), []byte("ref: refs/heads/topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCommonDir(t, wtGitDir, "../..")
	// Relative pointer, as git writes when the worktree lives under the repo.
	if err := os.WriteFile(filepath.Join(wtTree, ".git"), []byte("gitdir: ../../.git/worktrees/a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, ok := Detect(wtTree, "")
	if !ok {
		t.Fatal("ok=false for a relative gitdir pointer")
	}
	if st.Worktree != "a" || st.Branch != "topic" || st.Repo != "repo" {
		t.Errorf("got worktree=%q branch=%q repo=%q, want a/topic/repo", st.Worktree, st.Branch, st.Repo)
	}
}

// TestDetect_Submodule pins that a submodule reports its OWN branch with no
// worktree name. A submodule's ".git" file is also a gitdir pointer, but it
// targets "<parent>/.git/modules/<name>" — "modules", not "worktrees" — so
// worktreeIdentity must not mistake it for a linked worktree and attribute it
// to the parent repo. Layout verified against a real `git submodule add`.
func TestDetect_Submodule(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "parent")
	sub := filepath.Join(parent, "subm")
	modDir := filepath.Join(parent, ".git", "modules", "subm")
	for _, d := range []string{sub, modDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "HEAD"), []byte("ref: refs/heads/sub-branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Exactly what `git submodule add` writes (relative pointer).
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../.git/modules/subm\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, ok := Detect(sub, "")
	if !ok {
		t.Fatal("ok=false for a submodule checkout")
	}
	if st.Branch != "sub-branch" {
		t.Errorf("branch = %q, want the submodule's own branch sub-branch", st.Branch)
	}
	if st.Worktree != "" {
		t.Errorf("worktree = %q, want empty — a submodule is not a linked worktree", st.Worktree)
	}
	if st.Repo != "subm" {
		t.Errorf("repo = %q, want subm (its own dir), not the parent", st.Repo)
	}
}

func TestDetect_NotARepo(t *testing.T) {
	t.Parallel()
	if st, ok := Detect(t.TempDir(), ""); ok {
		t.Errorf("ok=true for a non-repo dir: %+v", st)
	}
}

func TestDetect_RejectsRelativeAndEmptyPaths(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "relative/path", "."} {
		if _, ok := Detect(in, ""); ok {
			t.Errorf("Detect(%q) = ok, want false", in)
		}
	}
}

func TestDetect_MalformedHeadIsNotARepo(t *testing.T) {
	t.Parallel()
	// A HEAD whose content is neither "ref: refs/…" nor a hex object id must
	// not be echoed back — that would make the endpoint a file-content oracle.
	for name, head := range map[string]string{
		"arbitrary text":    "AWS_SECRET_ACCESS_KEY=hunter2\n",
		"ref outside refs/": "ref: /etc/passwd\n",
		"empty":             "",
		"short hex":         "0123abc\n",
	} {
		root := t.TempDir()
		writeGitDir(t, root, head)
		if st, ok := Detect(root, ""); ok {
			t.Errorf("%s: ok=true (branch=%q sha=%q), want false", name, st.Branch, st.HeadSHA)
		}
	}
}

func TestDetect_MissingHeadIsNotARepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := Detect(root, ""); ok {
		t.Error("ok=true for a .git dir with no HEAD")
	}
}

func TestDetect_ControlBytesStrippedFromBranch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// git would never write this, but the value lands in slog attrs + HTML,
	// so the sanitiser must hold regardless of who wrote the file.
	writeGitDir(t, root, "ref: refs/heads/ma\x1b[31mster\n")

	st, ok := Detect(root, "")
	if !ok {
		t.Fatal("ok=false")
	}
	if strings.ContainsRune(st.Branch, 0x1b) {
		t.Errorf("Branch = %q still carries an ESC byte", st.Branch)
	}
	if st.Branch != "ma[31mster" {
		t.Errorf("Branch = %q, want the ESC stripped and the rest kept", st.Branch)
	}
}

func TestDetect_SymlinkedDotGitRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	t.Parallel()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeGitDir(t, real, "ref: refs/heads/master\n")

	fake := filepath.Join(base, "fake")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, ".git"), filepath.Join(fake, ".git")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// The walk-up must not treat the symlink as a checkout. `base` itself is
	// not a repo, so the whole lookup fails.
	if st, ok := Detect(fake, ""); ok {
		t.Errorf("ok=true through a .git symlink: %+v", st)
	}
}

func TestDetect_OverLongRefnameRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Pins maxRefNameBytes, NOT the read cap — a 1 MB refname fails the 255-byte
	// name check long before the 4 KiB read cap matters. See
	// TestDetect_HeadReadIsCapped for the read cap itself.
	writeGitDir(t, root, "ref: refs/heads/"+strings.Repeat("x", 1<<20))
	if _, ok := Detect(root, ""); ok {
		t.Error("ok=true for an over-long refname, want false")
	}
}

// TestReadFileCapped_StopsAtCap pins the maxHeadBytes memory bound at the layer
// where it is actually observable.
//
// It deliberately does NOT go through Detect: the cap has no reachable effect
// on Detect's OUTPUT, because any first line long enough to be truncated by the
// 4 KiB cap is also longer than the 255-byte maxRefNameBytes check, which
// rejects it first. The cap's job is purely "never read a huge file into
// memory", so that is what gets asserted — a byte count, not a parse result.
//
// The bound is a LITERAL, not maxHeadBytes: asserting `len <= maxHeadBytes`
// while passing maxHeadBytes as the argument moves the goalposts with the
// constant, so raising the cap to 1 GiB would still pass and pin nothing.
func TestReadFileCapped_StopsAtCap(t *testing.T) {
	t.Parallel()
	const wantMax = 4 << 10 // must track maxHeadBytes deliberately, not automatically
	if maxHeadBytes != wantMax {
		t.Fatalf("maxHeadBytes = %d, but this test pins %d — widening the cap means "+
			"more attacker-controlled bytes held in memory per request; update both "+
			"consciously", maxHeadBytes, wantMax)
	}
	path := filepath.Join(t.TempDir(), "big")
	// One long line with no newline, so nothing but the cap can bound the read.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := readFileCapped(path, maxHeadBytes)
	if err != nil {
		t.Fatalf("readFileCapped: %v", err)
	}
	if len(data) > wantMax {
		t.Errorf("read %d bytes from a 4 MiB file, want at most %d", len(data), wantMax)
	}
}

// TestReadFileCapped_RejectsDirectory pins the regular-file gate (a directory
// would otherwise read as garbage or error opaquely).
func TestReadFileCapped_RejectsDirectory(t *testing.T) {
	t.Parallel()
	if _, err := readFileCapped(t.TempDir(), maxHeadBytes); err == nil {
		t.Error("readFileCapped accepted a directory, want an error")
	}
}

func TestDetect_WalkUpIsBounded(t *testing.T) {
	t.Parallel()
	// Literal depth, deliberately NOT maxWalkUp+2: a relative depth tracks the
	// constant, so raising maxWalkUp 40→400 would keep passing and the bound
	// would not be pinned at all. 45 > the current 40, so widening the bound
	// must be a conscious edit here too.
	const depth = 45
	if depth <= maxWalkUp {
		t.Fatalf("fixture depth %d must exceed maxWalkUp %d to test the bound", depth, maxWalkUp)
	}
	root := t.TempDir()
	writeGitDir(t, root, "ref: refs/heads/master\n")
	deep := root
	for i := 0; i < depth; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := Detect(deep, ""); ok {
		t.Error("ok=true beyond maxWalkUp, want the walk to stop")
	}
}
