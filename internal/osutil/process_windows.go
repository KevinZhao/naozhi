//go:build windows

package osutil

import (
	"errors"
	"os"
	"syscall"
)

// PidAlive reports whether a process with the given PID exists (EPERM counts
// as alive). Windows build stub via os.FindProcess + Signal(0); naozhi is a
// Linux daemon (see signal_windows.go).
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
