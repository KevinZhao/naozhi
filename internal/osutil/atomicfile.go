package osutil

import (
	"fmt"
	"io/fs"
	"os"
)

// WriteFileAtomic writes data to path via the standard write-tmp → fsync →
// close → rename pattern used across the naozhi stores. It replaces repeated
// boilerplate in session/store.go, cron/store.go, project/project.go, and the
// router's workspace-overrides / known-IDs save paths.
//
// Semantics:
//   - The temp file is created at `path + ".tmp"` with mode perm.
//   - On any failure we best-effort-remove the temp file and wrap err with
//     context (the path is included so operators can locate the failing
//     file from logs).
//   - fsync is called before rename so a power cut after rename does not
//     leave a zero-byte file (ext4 data=ordered would still need this).
//   - Directory fsync is not performed; see TODO X2 — that is a separate,
//     FS-dependent decision.
//
// Callers still own mkdir of the parent directory so they can pick an
// appropriate permission and surface a distinct error (vs a write failure).
//
// Concurrency: the temp path is a fixed `path + ".tmp"`, so two concurrent
// calls with the same destination will race on the temp file. All in-tree
// callers serialise writes behind a per-store mutex; new callers must do
// the same or use a distinct destination path.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s to %s: %w", tmp, path, err)
	}
	return nil
}
