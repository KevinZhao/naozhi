//go:build windows

package ccassets

import (
	"io/fs"
	"os"
)

// openNoFollow is the windows shim: no O_NOFOLLOW, so Lstat rejects a
// final-component symlink before Open. A residual TOCTOU window remains
// between the two calls; naozhi's production target is Linux.
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
