package gitinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Commit resolves HEAD through a detached id, a loose ref, packed-refs and a
// linked worktree's common dir, and reads nothing it cannot name.
func TestCommit(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	write := func(t *testing.T, p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) string // returns the dir to ask
		want  string
	}{
		{"detached", func(t *testing.T, root string) string {
			writeGitDir(t, root, strings.ToUpper(a)+"\n")
			return root
		}, a},
		{"loose ref, from a subdirectory", func(t *testing.T, root string) string {
			writeGitDir(t, root, "ref: refs/heads/main\n")
			write(t, filepath.Join(root, ".git", "refs", "heads", "main"), a+"\n")
			write(t, filepath.Join(root, ".git", "packed-refs"), b+" refs/heads/main\n")
			return filepath.Join(root, "sub")
		}, a},
		{"packed ref", func(t *testing.T, root string) string {
			writeGitDir(t, root, "ref: refs/heads/main\n")
			write(t, filepath.Join(root, ".git", "packed-refs"), "# pack-refs with: peeled\n"+b+" refs/heads/other\n"+a+" refs/heads/main\n")
			return root
		}, a},
		{"linked worktree", func(t *testing.T, root string) string {
			common := filepath.Join(root, "main", ".git")
			wtGit := filepath.Join(common, "worktrees", "x")
			write(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/feat\n")
			writeCommonDir(t, wtGit, "../..")
			write(t, filepath.Join(common, "refs", "heads", "feat"), b+"\n")
			tree := filepath.Join(root, "wt")
			write(t, filepath.Join(tree, ".git"), "gitdir: "+wtGit+"\n")
			return tree
		}, b},
		{"unborn branch", func(t *testing.T, root string) string {
			writeGitDir(t, root, "ref: refs/heads/main\n")
			return root
		}, ""},
		{"ref escaping the git dir", func(t *testing.T, root string) string {
			writeGitDir(t, root, "ref: refs/../../secret\n")
			write(t, filepath.Join(root, "secret"), a+"\n")
			return root
		}, ""},
		{"not a repo", func(t *testing.T, root string) string { return root }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir := tt.setup(t, root)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if got := Commit(dir); got != tt.want {
				t.Errorf("Commit = %q, want %q", got, tt.want)
			}
		})
	}
	if got := Commit("relative"); got != "" {
		t.Errorf("Commit(relative) = %q", got)
	}
}
