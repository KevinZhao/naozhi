//go:build windows

package project

import "os"

// CreateWorkspaceFile is the windows shim: without O_NOFOLLOW a symlinked leaf
// cannot be refused atomically. O_EXCL still prevents silent overwrite and the
// handler's parent-dir EvalSymlinks + prefix check still contains the
// directory; naozhi's production target is Linux.
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
