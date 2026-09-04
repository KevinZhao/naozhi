//go:build !windows

package osutil

import (
	"errors"
	"syscall"
)

// PidAlive reports whether a process with the given PID exists (EPERM counts
// as alive). Non-positive PIDs are rejected: kill(0, 0) and kill(-N, 0)
// target process groups and would report a phantom "alive" for a
// zero/uninitialised PID.
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
