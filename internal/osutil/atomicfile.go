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
// Concurrency: the temp file name is generated via os.CreateTemp using a
// unique suffix, so two concurrent calls with the same destination cannot
// race on the temp file. Historic callers that took a per-store mutex may
// keep it for higher-level reasons (ordering of data written) but are not
// required to protect the temp file itself.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	// Pattern `.base.*.tmp` keeps temp files alongside the destination
	// (same filesystem, so rename is atomic) and visually groups them for
	// operators doing a crash-recovery sweep. The leading dot hides them
	// from default `ls` output.
	f, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if err := os.Chmod(tmp, perm); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s to %s: %w", tmp, path, err)
	}
	if err := SyncDir(dir); err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
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
