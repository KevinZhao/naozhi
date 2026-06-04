//go:build windows

package osutil

import (
	"errors"
	"os"
	"syscall"
)

// PidAlive checks whether a process with the given PID still exists.
// Returns true if the process is alive (or owned by another user — EPERM).
//
// Windows build-only stub: naozhi is a Linux daemon (see signal_windows.go);
// this file keeps GOOS=windows compilation green without changing runtime
// behaviour on the supported platform. Uses os.FindProcess + Signal(0) because
// syscall.Kill is not available on Windows.
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
