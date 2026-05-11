package osutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirSize_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	size, err := StateDirSize(dir)
	if err != nil {
		t.Fatalf("StateDirSize empty: %v", err)
	}
	if size != 0 {
		t.Fatalf("expected 0 bytes for empty dir, got %d", size)
	}
}

func TestStateDirSize_MissingDir(t *testing.T) {
	t.Parallel()
	// Non-existent path should return a non-nil error so callers can choose
	// to silently skip the warning on first-run systems.
	_, err := StateDirSize(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing dir, got nil")
	}
}

func TestStateDirSize_SumsFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// top-level file
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	// nested file
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world!"), 0600); err != nil {
		t.Fatal(err)
	}
	size, err := StateDirSize(dir)
	if err != nil {
		t.Fatalf("StateDirSize: %v", err)
	}
	want := int64(len("hello") + len("world!"))
	if size != want {
		t.Fatalf("size = %d, want %d", size, want)
	}
}

func TestStateDirSize_TruncatesOverBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create just over the budget worth of tiny files.
	total := stateDirWalkFileBudget + 50
	for i := 0; i < total; i++ {
		// Single-byte files keep the test fast.
		if err := os.WriteFile(filepath.Join(dir, name(i)), []byte{'x'}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	size, err := StateDirSize(dir)
	if !errors.Is(err, ErrStateDirScanTruncated) {
		t.Fatalf("expected ErrStateDirScanTruncated, got %v", err)
	}
	if size <= 0 {
		t.Fatalf("expected partial size > 0, got %d", size)
	}
}

func name(i int) string {
	// deterministic filename; uses a small alphabet to avoid fs limits
	const digits = "0123456789abcdef"
	b := make([]byte, 0, 8)
	if i == 0 {
		return "f0"
	}
	for i > 0 {
		b = append(b, digits[i&0xf])
		i >>= 4
	}
	return "f" + string(b)
}
