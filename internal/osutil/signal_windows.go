//go:build windows

package osutil

import "errors"

// SendTerm is a no-op on Windows: the shim + discovery stack is POSIX-only,
// and this stub lets cross-platform callers compile on GOOS=windows.
func SendTerm(pid int) error {
	return errors.ErrUnsupported
}

// SendShimReload is a no-op on Windows. Shim is POSIX-only; see SendTerm.
func SendShimReload(pid int) error {
	return errors.ErrUnsupported
}
