//go:build windows

package server

import "os"

// openWorkspaceFile is the windows shim — there's no O_NOFOLLOW, so we
// fall back to a plain Open. handleFileGet's Lstat-then-fstat-IsRegular
// check still rejects a swapped symlink targeting a non-regular file
// (device, fifo, dir); the residual same-regular-file inode-swap window
// matches the existing windows posture in openCronStoreFile / openRunFile
// shims. naozhi's production target is Linux. R219-SEC-2.
func openWorkspaceFile(resolved string) (*os.File, error) {
	return os.OpenFile(resolved, os.O_RDONLY, 0)
}
