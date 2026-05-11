//go:build !windows

package osutil

import "syscall"

// SendTerm sends SIGTERM to the given PID. Returns syscall.ESRCH if the
// process no longer exists; callers usually treat that as success. Wraps
// syscall.Kill so Windows builds can stub it out without polluting every
// caller with a build-tag block.
func SendTerm(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// SendShimReload sends SIGUSR2 — the shim's "immediate shutdown"
// signal — to the given PID. See internal/shim signal handler. Windows
// has no equivalent and stubs this out; the shim is POSIX-only.
func SendShimReload(pid int) error {
	return syscall.Kill(pid, syscall.SIGUSR2)
}
