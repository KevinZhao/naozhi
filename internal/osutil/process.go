package osutil

import (
	"errors"
	"os"
	"syscall"
)

// PidAlive checks whether a process with the given PID still exists.
// Returns true if the process is alive (or owned by another user — EPERM).
func PidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
