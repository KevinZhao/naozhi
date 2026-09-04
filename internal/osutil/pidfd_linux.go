//go:build linux

package osutil

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrPidReused is returned by SendTermVerified when the process at the given
// PID is not the instance identified by expectedStartTime any more — i.e. the
// original process exited and the kernel handed its PID to an unrelated
// process. Callers should surface this as a 409 Conflict rather than retrying.
var ErrPidReused = errors.New("process identity changed (PID reused)")

// SendTermVerified atomically confirms the target process identity and sends
// SIGTERM, closing the PID-reuse TOCTOU window of a separate alive-check →
// start_time check → kill sequence (#1670). pidfd_open(2) pins the exact
// process instance so pidfd_send_signal(2) can never reach a recycled PID;
// start_time is still re-read through the pinned identity so every platform
// shares the ErrPidReused contract. startTimeFn is injected (the caller
// supplies discovery.ProcStartTime) to avoid a reverse import.
//
// Returns nil (signalled), ErrPidReused (mismatch; nothing signalled),
// syscall.ESRCH (already exited; treat as success) or a pidfd error.
func SendTermVerified(pid int, expectedStartTime uint64, startTimeFn func(int) (uint64, error)) error {
	if pid <= 0 {
		return syscall.ESRCH
	}

	// Pin the process instance; PID recycling cannot alias the fd afterwards.
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		// ESRCH: already gone — success. EINVAL/ENOSYS (no pidfd support) is
		// surfaced so the caller can fall back rather than silently no-op.
		if errors.Is(err, syscall.ESRCH) {
			return syscall.ESRCH
		}
		return err
	}
	defer unix.Close(pidfd)

	// Identity guard on the pinned instance: if the PID was recycled before
	// PidfdOpen, the start_time does not match. expectedStartTime==0 is
	// rejected by the handler, but guard defensively.
	if expectedStartTime != 0 && startTimeFn != nil {
		actual, stErr := startTimeFn(pid)
		if stErr != nil {
			// Couldn't read start_time for the pinned process — it likely
			// exited between PidfdOpen and the read. Refuse to signal.
			return ErrPidReused
		}
		if actual != expectedStartTime {
			return ErrPidReused
		}
	}

	// Atomic, reuse-safe signal: targets the pinned instance only.
	if err := unix.PidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return syscall.ESRCH
		}
		return err
	}
	return nil
}
