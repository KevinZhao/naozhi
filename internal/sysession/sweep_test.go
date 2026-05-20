package sysession

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepOldJSONL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a mix of files: old jsonl (delete), new jsonl (keep), old
	// non-jsonl (keep — only .jsonl is in scope), new non-jsonl (keep).
	old := time.Now().Add(-30 * 24 * time.Hour)
	files := []struct {
		name     string
		mtime    time.Time
		expectGC bool
		desc     string
	}{
		{"a.jsonl", old, true, "old jsonl"},
		{"b.jsonl", time.Now(), false, "fresh jsonl"},
		{"c.txt", old, false, "old non-jsonl"},
		{"d.json", old, false, "old json (different ext)"},
	}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f.name, err)
		}
		if err := os.Chtimes(path, f.mtime, f.mtime); err != nil {
			t.Fatalf("chtimes %s: %v", f.name, err)
		}
	}

	deleted, err := SweepOldJSONL(dir, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepOldJSONL err = %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		_, statErr := os.Stat(path)
		exists := statErr == nil
		shouldExist := !f.expectGC
		if exists != shouldExist {
			t.Errorf("%s (%s): exists=%v, want %v", f.name, f.desc, exists, shouldExist)
		}
	}
}

func TestSweepOldJSONL_MissingDirIsZero(t *testing.T) {
	t.Parallel()
	deleted, err := SweepOldJSONL(filepath.Join(t.TempDir(), "nonexistent"), time.Hour)
	if err != nil {
		t.Errorf("missing dir should be no-op, got err=%v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}

func TestSweepOldJSONL_BadInputs(t *testing.T) {
	t.Parallel()
	if d, err := SweepOldJSONL("", time.Hour); d != 0 || err != nil {
		t.Errorf("empty dir should be no-op, got d=%d err=%v", d, err)
	}
	if d, err := SweepOldJSONL(t.TempDir(), 0); d != 0 || err != nil {
		t.Errorf("zero maxAge should be no-op, got d=%d err=%v", d, err)
	}
}

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
