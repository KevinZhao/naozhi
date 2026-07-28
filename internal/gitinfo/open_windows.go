//go:build windows

package gitinfo

import "os"

// openGitMeta is the windows shim. Windows has no O_NONBLOCK / O_NOFOLLOW, and
// named pipes are not reachable through a filesystem path the way a unix fifo
// is, so the blocking-open hazard the unix build guards against does not apply.
// The caller's fstat IsRegular check still rejects a directory or device.
// Mirrors the existing windows posture in project.OpenWorkspaceFile; naozhi's
// production target is Linux.
func openGitMeta(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}
