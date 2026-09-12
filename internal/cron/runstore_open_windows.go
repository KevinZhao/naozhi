//go:build windows

package cron

import (
	"fmt"
	"io/fs"
	"os"
)

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
