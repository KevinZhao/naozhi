//go:build !windows

package project

import (
	"os"
	"syscall"
)

// OpenWorkspaceFile opens resolved for reading without following a final-
// component symlink. Used by HandleFileGet so the bytes streamed by serve*
// helpers are guaranteed to come from the same inode that HandleFileGet
// already validated via Lstat — closing the R219-SEC-2 inode-swap TOCTOU
// where an attacker swapped the file for a symlink between Lstat and a
// later os.Open in serveRender / servePreview / serveRaw / serveDownload.
//
// O_NOFOLLOW: refuse to open a final-component symlink kernel-atomically;
// ELOOP is propagated unchanged so the caller can collapse it to 404 next
// to the existing "file not found" branch — the dashboard contract is
// "missing or escape attempt look identical".
//
// O_CLOEXEC: never let a forked child inherit the fd; mirrors openRunFile.
func OpenWorkspaceFile(resolved string) (*os.File, error) {
	return os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}
