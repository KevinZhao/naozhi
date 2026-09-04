//go:build windows

package gitinfo

import "os"

// openGitMeta is the windows shim. Windows has no O_NONBLOCK / O_NOFOLLOW and
// named pipes are not reachable through a filesystem path like a unix fifo, so
// the blocking-open hazard does not apply; fstat IsRegular still rejects devices.
func openGitMeta(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}
