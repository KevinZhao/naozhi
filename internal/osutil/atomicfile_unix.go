//go:build unix

package osutil

import (
	"errors"
	"syscall"
)

// IsDiskFull reports whether err wraps ENOSPC ("no space left on device") so
// callers can log disk-full distinctly from transient write failures.
func IsDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
