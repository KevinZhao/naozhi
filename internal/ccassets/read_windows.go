//go:build windows

package ccassets

import (
	"io/fs"
	"os"
)

// openNoFollow is the windows shim — there's no O_NOFOLLOW, so we use a
// best-effort Lstat→Open two-step: Lstat rejects a final-component symlink
// before we follow it, then Open returns the fd. There is a residual TOCTOU
// window between Lstat and Open, matching the existing windows posture in
// dashboard OpenWorkspaceFile and cron openRunFile shims. naozhi's production
// target is Linux. [R202606d-SEC-1]
func openNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: fs.ErrInvalid}
	}
	return os.OpenFile(path, os.O_RDONLY, 0)
}
