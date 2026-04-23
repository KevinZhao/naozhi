package osutil

import (
	"errors"
	"os"
	"syscall"
)

// PidAlive checks whether a process with the given PID still exists.
// Returns true if the process is alive (or owned by another user — EPERM).
//
// Reject non-positive PIDs explicitly: on Linux, kill(0, 0) broadcasts to
// the caller's process group (returning success even with no matching
// process), and kill(-N, 0) targets a process group which would report any
// live peer as "alive". Either can produce a phantom "alive" result when a
// caller has a zero/uninitialised PID (e.g. an incomplete shim hello).
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
