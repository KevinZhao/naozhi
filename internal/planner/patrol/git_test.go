package patrol

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// git runs the real git in dir, its commits dated at.
func git(t *testing.T, dir string, at time.Time, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	date := at.Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// clone is a repository at root/main with a bare remote beside it, one
// commit on master pushed 30 days before at.
func clone(t *testing.T, at time.Time) (root, main string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remote, main := filepath.Join(root, "remote.git"), filepath.Join(root, "main")
	old := at.Add(-30 * day)
	git(t, root, old, "init", "-q", "--bare", "-b", "master", remote)
	git(t, root, old, "init", "-q", "-b", "master", main)
	write(t, main, "a", "a", old)
	write(t, main, "b", "b", old)
	git(t, main, old, "add", "a", "b")
	git(t, main, old, "commit", "-q", "-m", "first")
	git(t, main, old, "remote", "add", "origin", remote)
	git(t, main, old, "push", "-q", "origin", "master")
	return root, main
}

// write puts text in dir/name, last touched at.
func write(t *testing.T, dir, name, text string, at time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// commit makes branch from master in dir with one commit dated at, and
// leaves master checked out.
func commit(t *testing.T, dir, branch string, at time.Time) {
	t.Helper()
	git(t, dir, at, "checkout", "-q", "-b", branch, "master")
	git(t, dir, at, "commit", "-q", "--allow-empty", "-m", branch)
	git(t, dir, at, "checkout", "-q", "master")
}

// look inspects dir with the real git, as the one directory of a workspace
// among dirs.
func look(t *testing.T, at time.Time, dir string, dirs ...workspace.Dir) dirReport {
	t.Helper()
	p := New(Config{Dir: t.TempDir(), Now: func() time.Time { return at }})
	rd := &round{now: at, dirs: dirs, repos: map[string]*repoReport{}}
	rep := p.inspect(context.Background(), rd, target{ID: wsA, Name: "repo", Dirs: []string{dir}}, dir)
	if rep.err != nil || rep.branchErr != nil {
		t.Fatalf("inspect: %v, %v", rep.err, rep.branchErr)
	}
	return rep
}

func texts(hits []hit) []string {
	out := []string{}
	for _, h := range hits {
		out = append(out, h.Text)
	}
	return out
}

// TestDirty: uncommitted changes hit once the one untouched the longest is
// 3 days old; an untracked .DS_Store, an ignored file and a deletion (which
// has no time) do not make it hit.
func TestDirty(t *testing.T) {
	at := time.Now()
	_, main := clone(t, at)
	if rep := look(t, at, main); len(rep.dirty) != 0 || rep.notRepo {
		t.Fatalf("a clean repository: %+v", rep)
	}
	// The check only reads. A file touched and not changed makes git status
	// refresh the index when it may, and here it may not: a session's own
	// git command would meet the lock.
	write(t, main, "b", "b", at.Add(-time.Hour))
	index, _ := os.ReadFile(filepath.Join(main, ".git", "index"))
	if rep := look(t, at, main); len(rep.dirty) != 0 {
		t.Errorf("a touched file hit: %v", texts(rep.dirty))
	}
	if after, _ := os.ReadFile(filepath.Join(main, ".git", "index")); len(index) == 0 || string(after) != string(index) {
		t.Error("the check rewrote the repository's index")
	}

	write(t, main, "a", "changed", at.Add(-2*day))
	write(t, main, ".DS_Store", "x", at.Add(-40*day))
	write(t, main, ".gitignore", "*.log\n", at.Add(-time.Hour))
	write(t, main, "old.log", "x", at.Add(-40*day))
	if err := os.Remove(filepath.Join(main, "b")); err != nil {
		t.Fatal(err)
	}
	if rep := look(t, at, main); len(rep.dirty) != 0 {
		t.Errorf("changes 2 days old hit: %v", texts(rep.dirty))
	}

	write(t, main, "c d.txt", "new", at.Add(-4*day-time.Hour))
	rep := look(t, at, main)
	// a, b (deleted), .gitignore and "c d.txt".
	if got, want := texts(rep.dirty), []string{"4 处改动没提交，最早的在 4 天前"}; !slices.Equal(got, want) {
		t.Fatalf("dirty = %v, want %v", got, want)
	}
	if rep.dirty[0].Source != "check:dirty:"+main {
		t.Errorf("source = %q", rep.dirty[0].Source)
	}

	// A staged rename is one change, at its new path.
	git(t, main, at, "checkout", "-q", "--", "a", "b")
	git(t, main, at, "mv", "a", "moved")
	if err := os.Chtimes(filepath.Join(main, "moved"), at.Add(-6*day), at.Add(-6*day)); err != nil {
		t.Fatal(err)
	}
	if got, want := texts(look(t, at, main).dirty), []string{"3 处改动没提交，最早的在 6 天前"}; !slices.Equal(got, want) {
		t.Errorf("with a rename dirty = %v, want %v", got, want)
	}

	// Only the changes below the registered directory are its own.
	sub := filepath.Join(main, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := texts(look(t, at, sub).dirty); len(got) != 0 {
		t.Errorf("a clean subdirectory of a dirty repository: %v", got)
	}
}

func TestStatusPaths(t *testing.T) {
	out := " M a.go\x00R  new name.go\x00old name.go\x00?? .DS_Store\x00?? docs/.DS_Store\x00 D gone.go\x00?? dir/\x00"
	want := []string{"a.go", "new name.go", "gone.go", "dir/"}
	if got := statusPaths(out); !slices.Equal(got, want) {
		t.Errorf("statusPaths = %q, want %q", got, want)
	}
}

// TestBranches: a branch pushed to origin whose last commit is 3 days old
// and that is not merged into the remote default branch hits, the oldest
// first; one merged, one never pushed, one that only tracks the default
// branch, one whose remote branch was deleted and a fresh one do not.
func TestBranches(t *testing.T) {
	at := time.Now()
	_, main := clone(t, at)
	old := at.Add(-4*day - time.Hour)

	commit(t, main, "feat", old)
	git(t, main, at, "push", "-q", "-u", "origin", "feat")
	// Pushed with no upstream set: its name on origin tells.
	commit(t, main, "plain", at.Add(-6*day))
	git(t, main, at, "push", "-q", "origin", "plain")
	commit(t, main, "local", old)
	commit(t, main, "fresh", at.Add(-day))
	git(t, main, at, "push", "-q", "-u", "origin", "fresh")
	// As `git worktree add -b` from origin/master leaves a branch.
	commit(t, main, "tracking", old)
	git(t, main, at, "branch", "-q", "--set-upstream-to=origin/master", "tracking")
	commit(t, main, "gone", old)
	git(t, main, at, "push", "-q", "-u", "origin", "gone")
	git(t, main, at, "push", "-q", "origin", "--delete", "gone")
	commit(t, main, "merged", old)
	git(t, main, at, "push", "-q", "-u", "origin", "merged")
	git(t, main, at, "merge", "-q", "--ff-only", "merged")
	git(t, main, at, "push", "-q", "origin", "master")
	// Cut from another branch of origin and never pushed: git makes that
	// branch its upstream from the start, and it is ahead of it.
	commit(t, main, "develop", old)
	git(t, main, at, "push", "-q", "origin", "develop")
	git(t, main, old, "checkout", "-q", "-b", "stacked", "origin/develop")
	git(t, main, old, "commit", "-q", "--allow-empty", "-m", "stacked")
	git(t, main, at, "checkout", "-q", "master")
	git(t, main, at, "branch", "-q", "-D", "develop")
	// Tracks a local branch, never pushed.
	git(t, main, old, "checkout", "-q", "-b", "local-track", "--track", "master")
	git(t, main, old, "commit", "-q", "--allow-empty", "-m", "local-track")
	git(t, main, at, "checkout", "-q", "master")
	// Pushed under another name, which it tracks.
	commit(t, main, "aliased", at.Add(-5*day))
	git(t, main, at, "push", "-q", "origin", "aliased:published")
	git(t, main, at, "branch", "-q", "--set-upstream-to=origin/published", "aliased")

	want := []string{
		"分支 plain 推上去了还没合进 master，最后的提交在 6 天前",
		"分支 aliased（远端是 published）推上去了还没合进 master，最后的提交在 5 天前",
		"分支 feat 推上去了还没合进 master，最后的提交在 4 天前",
	}
	rep := look(t, at, main)
	if got := texts(rep.branches); !slices.Equal(got, want) {
		t.Fatalf("branches = %v, want %v", got, want)
	}
	if rep.branches[2].Source != "check:branch:feat" || !strings.HasSuffix(rep.branches[2].Line, "（按本地上次 fetch 的状态）") {
		t.Errorf("hit = %+v", rep.branches[2])
	}
	if rep.branches[1].Source != "check:branch:aliased" {
		t.Errorf("a branch pushed under another name is told by %q, want its local name", rep.branches[1].Source)
	}
	if len(rep.notes) != 0 || rep.gh != nil {
		t.Errorf("notes = %v, gh = %v", rep.notes, rep.gh)
	}

	// With origin/HEAD set the default branch is read from it.
	git(t, main, at, "remote", "set-head", "origin", "master")
	if got := texts(look(t, at, main).branches); !slices.Equal(got, want) {
		t.Errorf("with origin/HEAD set branches = %v, want %v", got, want)
	}

	// Merged since: it no longer hits.
	git(t, main, at, "merge", "-q", "-m", "merge feat", "feat")
	git(t, main, at, "push", "-q", "origin", "master")
	if got := texts(look(t, at, main).branches); !slices.Equal(got, want[:2]) {
		t.Errorf("after the merge branches = %v, want %v", got, want[:2])
	}

	// The default branch ahead of origin's is not a branch pushed and left.
	git(t, main, old, "commit", "-q", "--allow-empty", "-m", "not pushed yet")
	if got := texts(look(t, at, main).branches); !slices.Equal(got, want[:2]) {
		t.Errorf("with master ahead of origin branches = %v, want %v", got, want[:2])
	}
}

// A repository whose origin has neither HEAD, main nor master says the
// branches were not looked at; one with nothing pushed says nothing.
func TestBranchesNoDefault(t *testing.T) {
	at := time.Now()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	remote, main := filepath.Join(root, "remote.git"), filepath.Join(root, "main")
	git(t, root, at, "init", "-q", "--bare", "-b", "trunk", remote)
	git(t, root, at, "init", "-q", "-b", "trunk", main)
	git(t, main, at, "commit", "-q", "--allow-empty", "-m", "first")
	if rep := look(t, at, main); len(rep.notes) != 0 || len(rep.branches) != 0 {
		t.Errorf("with no remote: %+v", rep)
	}
	git(t, main, at, "remote", "add", "origin", remote)
	git(t, main, at, "push", "-q", "origin", "trunk")
	if rep := look(t, at, main); len(rep.notes) != 1 || !strings.Contains(rep.notes[0], "找不到远端的默认分支") {
		t.Errorf("notes = %v", rep.notes)
	}
}

// TestNotARepository: a directory in no repository is not a failure; the
// worktrees a put-away left below it are still found.
func TestNotARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	left, gone := filepath.Join(dir, "repo", ".claude", "worktrees", "x"), filepath.Join(dir, "repo", ".claude", "worktrees", "gone")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	p := New(Config{Dir: t.TempDir(), Now: func() time.Time { return at }, Worktrees: func() []Worktree {
		return []Worktree{
			{Dir: left, GitDir: filepath.Join(dir, "repo", ".git"), Why: "有未提交的改动"},
			{Dir: left, GitDir: filepath.Join(dir, "repo", ".git"), Why: "有未提交的改动"},
			{Dir: gone, GitDir: filepath.Join(dir, "repo", ".git")},
			{Dir: filepath.Join(filepath.Dir(dir), "elsewhere", "wt"), GitDir: filepath.Join(filepath.Dir(dir), "elsewhere", ".git")},
		}
	}})
	rep := p.inspect(context.Background(), &round{now: at, repos: map[string]*repoReport{}}, target{ID: wsA, Dirs: []string{dir}}, dir)
	if !rep.notRepo || rep.err != nil {
		t.Fatalf("report = %+v", rep)
	}
	if got, want := texts(rep.worktrees), []string{"收起时留下的 worktree repo/.claude/worktrees/x（有未提交的改动）"}; !slices.Equal(got, want) {
		t.Errorf("worktrees = %v, want %v", got, want)
	}
	if lines := gitLines(rep); len(lines) != 2 || lines[0] != "不是 git 仓库" {
		t.Errorf("lines = %v", lines)
	}
}

// TestLinkedWorktree: a branch is reported by one directory — the
// registered one its worktree is in, else the main worktree — so worktrees
// of one repository registered as workspaces do not each file it.
func TestLinkedWorktree(t *testing.T) {
	at := time.Now()
	root, main := clone(t, at)
	old := at.Add(-5 * day)
	commit(t, main, "loose", old)
	git(t, main, at, "push", "-q", "-u", "origin", "loose")
	commit(t, main, "feat", old)
	git(t, main, at, "push", "-q", "-u", "origin", "feat")
	wt := filepath.Join(root, "wt")
	git(t, main, at, "worktree", "add", "-q", wt, "feat")
	commit(t, main, "inside", old)
	git(t, main, at, "push", "-q", "-u", "origin", "inside")
	git(t, main, at, "worktree", "add", "-q", filepath.Join(main, ".claude", "worktrees", "inside"), "inside")

	names := func(rep dirReport) []string {
		var out []string
		for _, h := range rep.branches {
			out = append(out, strings.TrimPrefix(h.Source, "check:branch:"))
		}
		slices.Sort(out)
		return out
	}
	both := []workspace.Dir{{ID: wsA, Path: main}, {ID: wsB, Path: wt}}
	if got, want := names(look(t, at, main, both...)), []string{"inside", "loose"}; !slices.Equal(got, want) {
		t.Errorf("the main worktree reports %v, want %v", got, want)
	}
	if got, want := names(look(t, at, wt, both...)), []string{"feat"}; !slices.Equal(got, want) {
		t.Errorf("the linked worktree reports %v, want %v", got, want)
	}
	// The linked worktree not registered: the main one reports its branch.
	if got, want := names(look(t, at, main, both[0])), []string{"feat", "inside", "loose"}; !slices.Equal(got, want) {
		t.Errorf("alone, the main worktree reports %v, want %v", got, want)
	}
}

// TestCaseVariantPaths: on a filesystem that folds case a registered path
// may spell a directory otherwise than git does (the registry keeps what
// the session came in by, git says what is on disk). The worktrees a
// put-away left are still found there, and a branch is still reported by
// the registered directory its worktree is in.
func TestCaseVariantPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk, registered := filepath.Join(root, "Repo"), filepath.Join(root, "repo")
	left := filepath.Join(disk, ".claude", "worktrees", "x")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if a, err := os.Stat(disk); err != nil {
		t.Fatal(err)
	} else if b, err := os.Stat(registered); err != nil || !os.SameFile(a, b) {
		t.Skip("the filesystem tells case apart")
	}
	p := New(Config{Dir: t.TempDir(), Worktrees: func() []Worktree {
		return []Worktree{{Dir: left, GitDir: filepath.Join(disk, ".git"), Why: "有未提交的改动"}}
	}})
	if got, want := texts(p.leftWorktrees(registered)), []string{"收起时留下的 worktree .claude/worktrees/x（有未提交的改动）"}; !slices.Equal(got, want) {
		t.Errorf("worktrees = %v, want %v", got, want)
	}

	other := filepath.Join(root, "other")
	dirs := []workspace.Dir{{ID: wsA, Path: other}, {ID: wsB, Path: registered}}
	if !reports(dirs, registered, false, disk) {
		t.Error("the registered directory does not report the branch checked out in it")
	}
	if reports(dirs, other, true, disk) {
		t.Error("the main worktree reports a branch whose worktree is a registered directory")
	}
}
