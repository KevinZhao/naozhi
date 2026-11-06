package reap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// repo is a main worktree with a bare remote and a linked worktree on
// branch feat; it returns the main and the linked directories.
func repo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	remote, main := filepath.Join(root, "remote.git"), filepath.Join(root, "main")
	git(t, root, "init", "-q", "--bare", "-b", "master", remote)
	git(t, root, "init", "-q", "-b", "master", main)
	if err := os.WriteFile(filepath.Join(main, "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, main, "add", "a")
	git(t, main, "commit", "-q", "-m", "a")
	git(t, main, "remote", "add", "origin", remote)
	git(t, main, "push", "-q", "origin", "master")
	wt := filepath.Join(main, ".claude", "worktrees", "feat")
	git(t, main, "worktree", "add", "-q", "-b", "feat", wt, "origin/master")
	return main, wt
}

func TestExecLinkedStatusRemoveAdd(t *testing.T) {
	ctx := context.Background()
	main, wt := repo(t)
	var g Exec
	if _, _, _, ok := g.Linked(ctx, main); ok {
		t.Error("the main worktree counted as linked")
	}
	sub := filepath.Join(wt, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	top, gitDir, branch, ok := g.Linked(ctx, sub)
	if !ok || top != wt || branch != "feat" || gitDir != filepath.Join(main, ".git") {
		t.Fatalf("Linked = %q %q %q %v", top, gitDir, branch, ok)
	}
	if err := os.Remove(sub); err != nil {
		t.Fatal(err)
	}

	st, err := g.Status(ctx, wt)
	if err != nil || st.Dirty || !st.Pushed {
		t.Fatalf("fresh worktree Status = %+v, %v; want clean and pushed", st, err)
	}
	if err := os.WriteFile(filepath.Join(wt, "b"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.Status(ctx, wt); !st.Dirty {
		t.Error("untracked file not counted as uncommitted")
	}
	if err := g.Remove(ctx, gitDir, wt); err == nil {
		t.Fatal("Remove removed a worktree with changes")
	}
	git(t, wt, "add", "b")
	git(t, wt, "commit", "-q", "-m", "b")
	if st, _ := g.Status(ctx, wt); st.Dirty || st.Pushed {
		t.Errorf("committed, not pushed: Status = %+v", st)
	}
	git(t, wt, "push", "-q", "-u", "origin", "feat")
	if st, _ := g.Status(ctx, wt); !st.Pushed {
		t.Errorf("pushed: Status = %+v", st)
	}

	if err := g.Remove(ctx, gitDir, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if err := g.Add(ctx, gitDir, wt, "feat"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "b")); err != nil {
		t.Errorf("re-added worktree lacks the branch's commit: %v", err)
	}
}
