package osutil

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// IsDiskFull is implemented per-platform (atomicfile_unix.go / atomicfile_nonunix.go).

// atomicTempPattern is the os.CreateTemp pattern WriteFileAtomic uses.
// ".<base>.*.tmp" keeps the temp file on the destination's filesystem (so the
// rename is atomic) and hidden from a default `ls`.
func atomicTempPattern(base string) string { return "." + base + ".*.tmp" }

// IsAtomicTempName reports whether a directory entry is a WriteFileAtomic temp
// file. A successful write renames its temp file into place and a failed one
// removes it, so one that outlives its write was stranded by a crash.
func IsAtomicTempName(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp")
}

// WriteFileAtomic writes data to path via write-tmp → fsync → close → rename.
// The temp file is created in path's directory with mode perm and removed on
// any failure; errors are wrapped with the path. The parent directory is
// fsynced after rename (XFS/tmpfs durability). A post-rename dir fsync
// failure is a soft degradation — the data is already atomically in place —
// so it is logged at WARN and nil is returned (#2279).
//
// Callers own mkdir of the parent directory. os.CreateTemp makes concurrent
// calls on the same destination safe without a caller mutex.
// syncDirFn indirects the directory fsync so tests can inject a failure.
var syncDirFn = SyncDir

func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	f, err := os.CreateTemp(dir, atomicTempPattern(base))
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
		// The rename already succeeded, so path holds the new data atomically;
		// a dir-entry fsync failure only risks the entry on an unclean crash on
		// XFS/tmpfs. Report success so callers do not treat a durable file as a
		// failed write (#2279).
		slog.Warn("osutil.WriteFileAtomic: dir fsync failed after rename; data is on disk, durability of dir entry degraded",
			"dir", dir, "path", path, "err", err)
	}
	return nil
}

// SyncDir fsyncs dir so a rename into it is durable on crash. EINVAL (FUSE /
// older filesystems that cannot fsync a directory) is swallowed silently; a
// permission-denied open is swallowed too but logged at Debug so a wrong-UID
// data dir can be correlated from logs (#730). Both are soft failures: the
// caller has already written + fsynced the data file itself.
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
