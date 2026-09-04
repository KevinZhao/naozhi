//go:build !unix

package osutil

// IsDiskFull always returns false on non-Unix platforms: Go does not remap
// Windows ERROR_DISK_FULL to syscall.ENOSPC, so callers see the generic
// "save failed" path. Exists so the package compiles on non-Unix builds.
func IsDiskFull(_ error) bool {
	return false
}
