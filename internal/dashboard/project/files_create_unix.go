//go:build !windows

package project

import (
	"errors"
	"os"
	"syscall"
)

// CreateWorkspaceFile creates (or, when overwrite is true, truncates) the
// leaf file at resolved for writing, refusing to follow a final-component
// symlink. It is the write-direction sibling of OpenWorkspaceFile and the
// security core of HandleFilesUpload.
//
// The caller MUST have already validated the PARENT directory via
// resolveProjectFileWithRoot (which EvalSymlinks the parent + prefix-checks
// it under the project root). The leaf itself is NOT pre-resolvable —
// EvalSymlinks needs the target to exist, and the whole point of an upload is
// that it does not yet — so the safety of the leaf is enforced atomically by
// the open flags here, not by a prior EvalSymlinks:
//
//   - O_NOFOLLOW: if the leaf already exists AND is a symlink, the kernel
//     fails with ELOOP rather than following it. This closes the
//     "write through a symlink the parent dir contains" escape — even on
//     opt-in overwrite, a symlinked leaf is refused, so an attacker cannot
//     redirect an overwrite onto an out-of-workspace target.
//   - O_EXCL (overwrite == false): the create fails with EEXIST if the leaf
//     already exists, so an upload never silently clobbers an existing file.
//     The handler surfaces EEXIST as 409.
//   - O_TRUNC (overwrite == true): truncate-in-place. O_NOFOLLOW is kept, so
//     even an explicit overwrite cannot follow a symlinked leaf. There is no
//     O_EXCL in this mode, so an existing regular file is replaced.
//   - O_CLOEXEC: never leak the fd to a forked child.
//
// Perm is 0o600 — uploaded files are never world/group-readable.
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
