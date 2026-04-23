package osutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic_CreatesAndReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	if err := WriteFileAtomic(path, []byte("first"), 0600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "first" {
		t.Fatalf("read = %q, err=%v", got, err)
	}

	if err := WriteFileAtomic(path, []byte("second"), 0600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "second" {
		t.Fatalf("read = %q, err=%v", got, err)
	}

	// tmp file should not linger.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file lingered: err=%v", err)
	}
}

func TestSyncDir_Exists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := SyncDir(dir); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
}

func TestSyncDir_Missing(t *testing.T) {
	t.Parallel()
	if err := SyncDir("/no/such/dir/here"); err == nil {
		t.Fatal("expected error for nonexistent dir")
	}
}
