//go:build windows

package jsonfile

import (
	"errors"
	"io/fs"
	"os"
)

// errSymlinkWindows stands in for syscall.ELOOP: windows has no O_NOFOLLOW, so
// openNoFollow is a best-effort Lstat→Open two-step with a residual TOCTOU
// window. Load's Fstat still validates IsRegular() on the fd. The production
// target is Linux; this exists so the package compiles on windows CI.
var errSymlinkWindows = errors.New("jsonfile: refused to follow symlink (windows shim)")

func openNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, errSymlinkWindows
	}
	return os.OpenFile(path, os.O_RDONLY, 0)
}

func isSymlinkErr(err error) bool { return errors.Is(err, errSymlinkWindows) }
