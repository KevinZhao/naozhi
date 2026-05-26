//go:build windows

package cron

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// errSymlinkLoopWindows is the windows-side stand-in for syscall.ELOOP.
// Returned by openCronStoreFile when Lstat catches a final-component
// symlink; isSymlinkLoopErr identifies it for the cross-platform caller.
var errSymlinkLoopWindows = errors.New("cron: refused to follow symlink (windows shim)")

// openRunFile opens path for reading. Windows lacks O_NOFOLLOW, so we use a
// best-effort Lstat→Open two-step: Lstat rejects a final-component symlink
// before we follow it, then Open returns the fd. There is a TOCTOU window
// between Lstat and Open in which an attacker could swap the entry to a
// symlink — Fstat in the caller still validates IsRegular() on the fd, so
// the worst case is opening a symlink target that happens to be a regular
// file. naozhi's production target is Linux; this path exists only so the
// package compiles on the windows-latest CI runner and developer workstations.
func openRunFile(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: refused to follow symlink", ErrCorruptRun)
	}
	return os.OpenFile(path, os.O_RDONLY, 0)
}

// openCronStoreFile is the cron_jobs.json store-side counterpart to the
// runstore openRunFile shim. Windows lacks O_NOFOLLOW so we keep the
// historical Lstat→Open shape; the residual TOCTOU is the same one that
// affected openRunFile on windows since R235-SEC-5, which the unix build
// closes via O_NOFOLLOW. naozhi's production target is Linux.
func openCronStoreFile(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, errSymlinkLoopWindows
	}
	return os.OpenFile(path, os.O_RDONLY, 0)
}

// isSymlinkLoopErr reports the windows shim's symlink sentinel.
func isSymlinkLoopErr(err error) bool {
	return errors.Is(err, errSymlinkLoopWindows)
}
