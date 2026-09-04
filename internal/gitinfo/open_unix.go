//go:build !windows

package gitinfo

import (
	"os"
	"syscall"
)

// openGitMeta opens a git metadata file for reading without blocking and
// without following a final-component symlink. O_NONBLOCK: opening a FIFO
// blocks inside open(2) until a writer appears, so a `.git/HEAD` fifo would pin
// a handler goroutine forever before the caller's fstat check could run.
// O_NOFOLLOW: refuse a symlinked HEAD kernel-atomically (mirrors
// project.OpenWorkspaceFile). O_CLOEXEC: no forked child inherits the fd.
func openGitMeta(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}
