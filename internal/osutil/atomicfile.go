package osutil

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// IsDiskFull is implemented per-platform: see atomicfile_unix.go (matches
// against syscall.ENOSPC) and atomicfile_nonunix.go (returns false — the
// stdlib's errno mapping for Windows ERROR_DISK_FULL does not surface as
// syscall.ENOSPC, and naozhi currently only runs on Linux, so the honest
// answer on non-Unix is "unknown, treat as non-disk-full").

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
//     lost store file on crash). The rename has already succeeded by this
//     point, so a SyncDir failure is a SOFT degradation (the data is
//     atomically in place and survives a clean reboot; only an unclean
//     crash before the dir entry flushes could lose it on XFS/tmpfs). We
//     therefore log it at WARN and return nil rather than a hard error —
//     R202606e-GO-003 (#2279): a hard error here made callers (e.g. cron
//     sandbox_pending) treat a fully-persisted file as a write failure and
//     skip index registration, defeating restart reconcile for a file that
//     is actually on disk.
//
// Callers still own mkdir of the parent directory so they can pick an
// appropriate permission and surface a distinct error (vs a write failure).
//
// Concurrency: the temp file name is generated via os.CreateTemp using a
// unique suffix, so two concurrent calls with the same destination cannot
// race on the temp file. Historic callers that took a per-store mutex may
// keep it for higher-level reasons (ordering of data written) but are not
// required to protect the temp file itself.
// syncDirFn is the directory-fsync step, indirected through a package var so
// tests can inject a failure to exercise the post-rename soft-degradation path
// (#2279) without a fault-injecting filesystem. Production wiring is SyncDir.
var syncDirFn = SyncDir

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
	if err := syncDirFn(dir); err != nil {
		// R202606e-GO-003 (#2279): the rename above already succeeded, so
		// `path` now holds the new data atomically. A directory-entry fsync
		// failure only risks losing that entry on an UNCLEAN crash on
		// XFS/tmpfs — it does NOT mean the write failed. Returning a hard
		// error here caused callers to treat a durable file as failed and
		// skip downstream bookkeeping (cron restart-reconcile index). Log
		// and report success: the data the caller asked us to write is on
		// disk.
		slog.Warn("osutil.WriteFileAtomic: dir fsync failed after rename; data is on disk, durability of dir entry degraded",
			"dir", dir, "path", path, "err", err)
	}
	return nil
}

// SyncDir fsyncs the given directory so a rename into it is durable on
// crash. On filesystems where opening a directory for sync is unsupported
// (e.g. some older FUSE backends), the open error is treated as a soft
// failure and swallowed — the caller has already written + fsynced the
// data file; a lost directory entry on crash is acceptable degradation.
//
// R237-CR-15 (#730): fs.ErrPermission previously returned nil with no
// trace. Real config issues (wrong UID on the data dir) silently lost
// the directory-entry fsync. We still swallow the error so the rename
// already on disk is reported as success, but emit slog.Debug so an
// operator scanning logs can correlate "data dir fsync skipped"
// against a misconfigured deployment. EINVAL stays silent because it
// is the documented FUSE/older-fs soft-failure path.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			slog.Debug("osutil.SyncDir: open denied, skipping fsync",
				"dir", dir, "err", err)
			return nil
		}
		if errors.Is(err, syscall.EINVAL) {
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
