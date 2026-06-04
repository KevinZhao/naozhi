//go:build !linux

package osutil

import (
	"errors"
	"syscall"
)

// ErrPidReused mirrors the Linux definition so callers compile and branch
// identically on every platform. See pidfd_linux.go for the full contract.
var ErrPidReused = errors.New("process identity changed (PID reused)")

// SendTermVerified is the non-Linux fallback for the atomic Linux pidfd path
// (#1670). pidfd_open/pidfd_send_signal are Linux-only, so here we retain the
// best-effort PID+start_time sequence: alive-check (via Signal(0)) → start_time
// identity guard → SendTerm. This still narrows the TOCTOU window relative to
// no guard, and the production target (Amazon Linux 2023) always takes the
// Linux path. darwin/windows are dev/test only for this code path.
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
