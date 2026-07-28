//go:build !windows

package gitinfo

import (
	"os"
	"syscall"
)

// openGitMeta opens a git metadata file (HEAD, the `.git` pointer file) for
// reading without ever blocking and without following a final-component
// symlink.
//
// O_NONBLOCK: opening a FIFO for reading blocks inside open(2) until a writer
// appears. A `.git/HEAD` fifo in an operator workspace would therefore pin a
// dashboard handler goroutine forever — and the caller's fstat regular-file
// check could never run, because open never returns. O_NONBLOCK makes the open
// return immediately so the mode check can reject it. No-op for regular files.
//
// O_NOFOLLOW: refuse a symlinked HEAD kernel-atomically, so the bytes come from
// the inode inside the git dir rather than wherever a link points. Mirrors
// project.OpenWorkspaceFile (R219-SEC-2).
//
// O_CLOEXEC: never let a forked child inherit the fd.
func openGitMeta(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}
