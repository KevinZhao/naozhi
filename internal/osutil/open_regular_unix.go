//go:build unix

package osutil

import (
	"os"
	"syscall"
)

// OpenRegular opens path read-only, accepting only a regular file, and returns
// the fd the caller must read through. O_NONBLOCK keeps a FIFO or device from
// blocking open(2) before the Fstat check can reject it (ErrNotRegular);
// O_NOFOLLOW refuses a final-component symlink. maxBytes > 0 is a size cap
// (ErrTooLarge); a caller reading only a tail passes 0 and uses ReadAt.
func OpenRegular(path string, maxBytes int64) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return checkRegular(f, maxBytes)
}

// openInFlags are OpenRegularIn's: os.Root already refuses a symlink that
// leaves the root, and O_NONBLOCK keeps a FIFO from blocking the open.
const (
	openInFlags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	openDirFlag = syscall.O_DIRECTORY
)
