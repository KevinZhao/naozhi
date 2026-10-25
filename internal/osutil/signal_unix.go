//go:build !windows

package osutil

import "syscall"

// SendTerm sends SIGTERM to the given PID. Returns syscall.ESRCH if the
// process does not exist; callers usually treat that as success.
func SendTerm(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// SendShimReload sends SIGUSR2 — the shim's "immediate shutdown"
// signal — to the given PID. See internal/shim signal handler.
func SendShimReload(pid int) error {
	return syscall.Kill(pid, syscall.SIGUSR2)
}
