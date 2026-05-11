//go:build windows

package osutil

import "errors"

// SendTerm is a no-op on Windows. The naozhi shim + discovery stack is
// POSIX-only (release.yml matrix excludes windows); this stub lets
// cross-platform callers in internal/upstream and internal/server
// compile on GOOS=windows without conditional code at every call site.
// Runtime is unreachable on windows because discovery.ProcStartTime
// and the shim handshake both refuse to operate.
func SendTerm(pid int) error {
	return errors.ErrUnsupported
}

// SendShimReload is a no-op on Windows. Shim is POSIX-only; see SendTerm.
func SendShimReload(pid int) error {
	return errors.ErrUnsupported
}
