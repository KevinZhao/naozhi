//go:build !windows

package jsonfile

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow opens path read-only with O_NOFOLLOW (a final-component symlink
// fails with ELOOP — kernel-atomic, no TOCTOU window) and O_CLOEXEC. Errors are
// returned raw; Load classifies ErrNotExist / ELOOP itself.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}

// isSymlinkErr reports the kernel's "refused to follow symlink" signal.
func isSymlinkErr(err error) bool { return errors.Is(err, syscall.ELOOP) }
