//go:build !windows

package project

import (
	"os"
	"syscall"
)

// OpenWorkspaceFile opens resolved for reading without following a
// final-component symlink, so the bytes the serve* helpers stream come from
// the same inode HandleFileGet validated via Lstat (closes the inode-swap
// TOCTOU). ELOOP is propagated unchanged so the caller can collapse it to 404
// — "missing" and "escape attempt" must look identical. O_CLOEXEC keeps the
// fd from forked children.
func OpenWorkspaceFile(resolved string) (*os.File, error) {
	return os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}
