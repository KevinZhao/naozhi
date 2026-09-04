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

// openRunFile opens path for reading. Windows lacks O_NOFOLLOW, so this is a
// best-effort Lstat→Open two-step with a residual TOCTOU window; the caller's
// Fstat still validates IsRegular() on the fd. Production target is Linux;
// this exists so the package compiles on windows CI and workstations.
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

// openCronStoreFile is the cron_jobs.json counterpart to openRunFile: same
// best-effort Lstat→Open shape and residual TOCTOU (Linux closes it with O_NOFOLLOW).
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
