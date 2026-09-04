//go:build windows

package project

import "os"

// OpenWorkspaceFile is the windows shim: without O_NOFOLLOW it falls back to a
// plain Open. HandleFileGet's fstat IsRegular check still rejects a swap to a
// non-regular file; the residual same-regular-file inode-swap window is
// accepted because naozhi's production target is Linux.
func OpenWorkspaceFile(resolved string) (*os.File, error) {
	return os.OpenFile(resolved, os.O_RDONLY, 0)
}
