//go:build !linux

package osutil

import (
	"errors"
	"syscall"
)

// ErrPidReused mirrors the Linux definition so callers compile and branch
// identically on every platform. See pidfd_linux.go for the full contract.
var ErrPidReused = errors.New("process identity changed (PID reused)")

// SendTermVerified is the non-Linux fallback (#1670): pidfd is Linux-only, so
// this keeps the best-effort alive-check → start_time guard → SendTerm
// sequence. Production (Amazon Linux 2023) always takes the Linux path.
func SendTermVerified(pid int, expectedStartTime uint64, startTimeFn func(int) (uint64, error)) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	if !PidAlive(pid) {
		return syscall.ESRCH
	}
	if expectedStartTime != 0 && startTimeFn != nil {
		actual, err := startTimeFn(pid)
		if err != nil || actual != expectedStartTime {
			return ErrPidReused
		}
	}
	return SendTerm(pid)
}
