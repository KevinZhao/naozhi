//go:build !unix

package osutil

// IsDiskFull is a no-op on non-Unix platforms. Windows raises a
// different errno for "disk full" (ERROR_DISK_FULL /
// ERROR_HANDLE_DISK_FULL) and Go's stdlib does not remap them to
// syscall.ENOSPC; returning false here keeps the behaviour explicit
// and conservative: callers still see the underlying error from
// WriteFileAtomic and will log it at the generic "save failed" level
// rather than the disk-full-specific code path.
//
// naozhi is currently Linux-only in deployment, so this stub exists
// purely so the package compiles cleanly on non-Unix builds (CI on
// a Windows runner, local `go build` from a Windows dev machine,
// future cross-compile probes). OBS2.
func IsDiskFull(_ error) bool {
	return false
}
