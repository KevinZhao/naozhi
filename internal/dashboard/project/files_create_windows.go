//go:build windows

package project

import "os"

// CreateWorkspaceFile is the windows shim — there is no O_NOFOLLOW, so a
// symlinked leaf cannot be refused kernel-atomically. We approximate the
// unix posture: O_EXCL still prevents silent overwrite on create, and the
// handler's prior parent-dir EvalSymlinks + prefix check still contains the
// directory. The residual symlinked-leaf-overwrite window matches the
// existing windows posture in OpenWorkspaceFile; naozhi's production target
// is Linux where the O_NOFOLLOW path is authoritative.
func CreateWorkspaceFile(resolved string, overwrite bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE
	if overwrite {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	return os.OpenFile(resolved, flags, 0o600)
}

// isSymlinkLoopErr is always false on windows — there is no O_NOFOLLOW ELOOP
// path. Present so the shared upload handler compiles on both platforms.
func isSymlinkLoopErr(error) bool { return false }
