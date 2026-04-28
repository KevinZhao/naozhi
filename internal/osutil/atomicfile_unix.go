//go:build unix

package osutil

import (
	"errors"
	"syscall"
)

// IsDiskFull reports whether err is a "no space left on device" error
// (ENOSPC) from any level of the error chain. Callers emit a distinct
// structured log field so monitoring can page on disk-full separately
// from transient write failures.
//
// Unix-only: the Go stdlib surfaces ENOSPC as syscall.ENOSPC on
// Linux/Darwin/FreeBSD/etc. On Windows the underlying error is
// ERROR_DISK_FULL / ERROR_HANDLE_DISK_FULL (distinct from ENOSPC);
// see atomicfile_nonunix.go for the stub. Splitting by build tag
// follows the existing sdnotify_linux.go / sdnotify_other.go
// precedent in this package. OBS2.
func IsDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
