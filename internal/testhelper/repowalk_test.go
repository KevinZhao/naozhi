package testhelper

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeRepoTree builds a checkout named naozhi holding every kind of
// directory a repository walk meets, and returns its path.
func writeRepoTree(t *testing.T) string {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "naozhi")
	for _, p := range []string{
		".git/HEAD",
		"cmd/naozhi/main.go",
		"internal/a/a.go",
		"internal/a/testdata/x.go",
		"internal/testhelper/h.go",
		"test/e2e/node_modules/x/x.js",
		"vendor/x/x.go",
		"naozhi/internal/a/a.go",
		".claude/worktrees/agent-x/.git",
		".claude/worktrees/agent-x/internal/a/a.go",
		"wt/.git",
		"wt/internal/a/a.go",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// walkedDirs returns the directories a walk from root enters, relative to
// base and slash-separated.
func walkedDirs(t *testing.T, root, base string) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if SkipRepoDir(root, path, d) {
			return filepath.SkipDir
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, abs)
		if err != nil {
			return err
		}
		dirs = append(dirs, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(dirs)
	return dirs
}

// TestSkipRepoDir walks the same tree from an absolute root and from the
// relative roots the ratchets use. "." and "../.." start with a dot, and the
// root holds a .git entry, so only the root guard keeps those walks from
// going blind.
func TestSkipRepoDir(t *testing.T) {
	root := writeRepoTree(t)
	want := []string{".", "cmd", "cmd/naozhi", "internal", "internal/a", "internal/testhelper", "test", "test/e2e"}

	if got := walkedDirs(t, root, root); !slices.Equal(got, want) {
		t.Errorf("absolute root: walked %v, want %v", got, want)
	}
	for _, c := range []struct{ cwd, root string }{
		{".", "."},
		{"internal/testhelper", filepath.Join("..", "..")},
	} {
		t.Run(c.root, func(t *testing.T) {
			t.Chdir(filepath.Join(root, c.cwd))
			if got := walkedDirs(t, c.root, root); !slices.Equal(got, want) {
				t.Errorf("walked %v, want %v", got, want)
			}
		})
	}
}
