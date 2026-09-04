//go:build !windows

package project

import (
	"errors"
	"os"
	"syscall"
)

// CreateWorkspaceFile creates (or, with overwrite, truncates) the leaf file at
// resolved for writing, refusing to follow a final-component symlink; it is
// the security core of HandleFilesUpload. The caller MUST have validated the
// PARENT via resolveProjectFileWithRoot; the leaf cannot be pre-resolved (it
// may not exist yet), so its safety is enforced atomically by the flags:
// O_NOFOLLOW (an existing symlinked leaf fails with ELOOP, even on overwrite),
// O_EXCL without overwrite (EEXIST → 409, never a silent clobber), O_TRUNC
// with overwrite, O_CLOEXEC. Perm 0o600 — uploads are never group/world-readable.
func CreateWorkspaceFile(resolved string, overwrite bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if overwrite {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	return os.OpenFile(resolved, flags, 0o600)
}

// isSymlinkLoopErr reports whether err is the ELOOP the kernel returns when
// O_NOFOLLOW refuses a final-component symlink. The upload handler collapses
// it to a 409 so a symlinked leaf is indistinguishable from an ordinary
// already-exists conflict (no oracle).
func isSymlinkLoopErr(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
