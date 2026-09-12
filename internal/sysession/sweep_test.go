package sysession

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureWorkDir_CreatesAndChmod(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "sub", "deeper", "sys-sessions")
	abs, err := EnsureWorkDir(target)
	if err != nil {
		t.Fatalf("EnsureWorkDir err = %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat created dir: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o700 {
		t.Errorf("dir mode = %o, want 0700", mode)
	}
}

func TestEnsureWorkDir_ChmodsExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Loosen perms to simulate a pre-v2.1 leftover.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWorkDir(dir); err != nil {
		t.Fatalf("EnsureWorkDir err = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("after EnsureWorkDir, mode = %o, want 0700", mode)
	}
}
