package osutil

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// TestWriteFileAtomic_ConcurrentSameDest confirms that concurrent writers to
// the same destination path do not corrupt each other's temp files. Before
// the switch to os.CreateTemp the fixed `.tmp` suffix meant two racers would
// O_TRUNC each other's in-flight bytes and the rename loser would surface a
// torn file to the next reader.
func TestWriteFileAtomic_ConcurrentSameDest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "contended.json")

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf("writer-%d", idx))
			if err := WriteFileAtomic(path, payload, 0600); err != nil {
				t.Errorf("writer %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	// The winner's payload must be one of the writers, byte-for-byte; a torn
	// write would produce partial bytes or empty file.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	ok := false
	for i := 0; i < writers; i++ {
		if string(got) == fmt.Sprintf("writer-%d", i) {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatalf("final contents do not match any writer: %q", got)
	}

	// No lingering temp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "contended.json" {
			t.Errorf("unexpected residual file: %s", e.Name())
		}
	}
}
