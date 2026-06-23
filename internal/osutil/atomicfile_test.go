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

// TestWriteFileAtomic_SyncDirFailureIsSoft pins R202606e-GO-003 (#2279): the
// os.Rename has already succeeded by the time SyncDir runs, so a dir-fsync
// failure must NOT propagate as a hard error — the data is durably in place.
// A hard error here made callers (cron sandbox_pending) treat a fully written
// file as a write failure and skip restart-reconcile index registration.
func TestWriteFileAtomic_SyncDirFailureIsSoft(t *testing.T) {
	// Not parallel: mutates the package-level syncDirFn.
	orig := syncDirFn
	t.Cleanup(func() { syncDirFn = orig })
	syncDirFn = func(string) error { return fmt.Errorf("injected dir fsync failure") }

	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	if err := WriteFileAtomic(path, []byte("payload"), 0600); err != nil {
		t.Fatalf("WriteFileAtomic: want nil despite SyncDir failure (data already renamed), got %v", err)
	}
	// Data must be on disk: the rename happened before SyncDir.
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "payload" {
		t.Fatalf("read = %q, err=%v; want %q", got, err, "payload")
	}
	// No lingering temp file.
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

// TestSyncDir_PermissionDeniedSwallowed pins R237-CR-15 (#730): a
// permission-denied open on the directory still returns nil (the
// rename target is already on disk; degraded fsync of the directory
// entry is acceptable), but should not panic and should log at debug.
// We can only reproduce ErrPermission if the test isn't running as
// root — skip otherwise so CI under root doesn't false-pass.
func TestSyncDir_PermissionDeniedSwallowed(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 still allows open(2)")
	}
	dir := t.TempDir()
	inner := filepath.Join(dir, "locked")
	if err := os.Mkdir(inner, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(inner, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(inner, 0700) })

	if err := SyncDir(inner); err != nil {
		t.Fatalf("SyncDir on chmod-0 dir: want nil (soft failure), got %v", err)
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
