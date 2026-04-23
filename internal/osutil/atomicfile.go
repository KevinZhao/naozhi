package osutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// IsDiskFull reports whether err is a "no space left on device" error
// (ENOSPC) from any level of the error chain. Callers can emit a
// distinct structured log field so monitoring can page on disk-full
// separately from transient write failures.
func IsDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

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
//   - After rename, the parent directory is fsynced so the new directory
//     entry is persisted on XFS/tmpfs (ext4 data=ordered usually handles
//     this automatically, but paying one extra syscall is cheap vs. a
//     lost store file on crash). Failure to sync the directory is logged
//     by the caller context via the wrapped error but does not undo the
//     rename — the data is already on disk.
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
	if err := SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("fsync dir %s: %w", filepath.Dir(path), err)
	}
	return nil
}

// SyncDir fsyncs the given directory so a rename into it is durable on
// crash. On filesystems where opening a directory for sync is unsupported
// (e.g. some older FUSE backends), the open error is treated as a soft
// failure and swallowed — the caller has already written + fsynced the
// data file; a lost directory entry on crash is acceptable degradation.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EINVAL) {
			return nil
		}
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) {
			return nil
		}
		return err
	}
	return nil
}
