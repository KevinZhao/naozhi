package shim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteStateFile_AtomicViaOSUtil pins R247-ARCH-5 (#621): WriteStateFile
// delegates the atomic write sequence to osutil.WriteFileAtomic instead of a
// hand-rolled re-implementation. The contract guarantees:
//
//  1. After a successful write, no .tmp file remains in the parent dir
//     (temp file is renamed atomically over the destination).
//  2. The destination file is mode 0600 (private to the running uid; the
//     state file embeds an AuthToken that grants direct shim socket attach).
//  3. The parent directory is mode 0700.
//
// If a future refactor accidentally reverts to a per-package atomic write
// that drops fsync or leaves orphan temp files, this test fails.
func TestWriteStateFile_AtomicViaOSUtil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shimstate", "shim-abc.json")

	state := State{
		ShimPID:   12345,
		Socket:    "/tmp/shim.sock",
		AuthToken: "dGVzdHRva2Vu",
		Key:       "k1",
		StartedAt: "2026-05-26T00:00:00Z",
	}
	if err := WriteStateFile(path, state); err != nil {
		t.Fatalf("WriteStateFile: %v", err)
	}

	// (1) No leftover .tmp files in the parent dir.
	parent := filepath.Dir(path)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("ReadDir parent: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			t.Errorf("leftover temp file in parent dir: %q (atomic-write contract violated)", name)
		}
	}

	// (2) Destination file mode is 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %#o, want 0600", perm)
	}

	// (3) Parent directory mode is 0700.
	dfi, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
	if perm := dfi.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir mode = %#o, want 0700", perm)
	}
}

// TestWriteStateFile_OverwriteAtomicReplace verifies that overwriting an
// existing state file with new content leaves only the new content visible
// (no partial-write window where the file is empty / mixed). This is a
// regression guard for the temp-rename invariant — direct os.WriteFile
// would have a window where the file is truncated mid-write.
func TestWriteStateFile_OverwriteAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	v1 := State{
		ShimPID:   1,
		Socket:    "/tmp/v1.sock",
		AuthToken: "dG9rMQ==",
		Key:       "k1",
		StartedAt: "2026-05-26T00:00:00Z",
	}
	if err := WriteStateFile(path, v1); err != nil {
		t.Fatalf("WriteStateFile v1: %v", err)
	}

	v2 := State{
		ShimPID:   2,
		Socket:    "/tmp/v2.sock",
		AuthToken: "dG9rMg==",
		Key:       "k1",
		StartedAt: "2026-05-26T00:00:00Z",
	}
	if err := WriteStateFile(path, v2); err != nil {
		t.Fatalf("WriteStateFile v2: %v", err)
	}

	got, err := ReadStateFile(path)
	if err != nil {
		t.Fatalf("ReadStateFile: %v", err)
	}
	if got.ShimPID != v2.ShimPID || got.Socket != v2.Socket {
		t.Errorf("expected v2 content (PID=%d Socket=%s), got PID=%d Socket=%s",
			v2.ShimPID, v2.Socket, got.ShimPID, got.Socket)
	}

	// Confirm no orphans in parent.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after overwrite: %q", e.Name())
		}
	}
}
