//go:build linux

package osutil

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrPidReused is returned by SendTermVerified when the process at the given
// PID is no longer the instance identified by expectedStartTime — i.e. the
// original process exited and the kernel handed its PID to an unrelated
// process. Callers should surface this as a 409 Conflict rather than retrying.
var ErrPidReused = errors.New("process identity changed (PID reused)")

// SendTermVerified atomically confirms the target process identity and sends
// SIGTERM, closing the PID-reuse TOCTOU window that the previous
// PidAlive → verifyProcIdent → SendTerm sequence left open. (#1670)
//
// The previous three-step sequence was non-atomic: between the start_time
// identity check and the kill, the target could exit and the kernel reuse its
// PID for an unrelated process. start_time alone is defence-in-depth only
// (Linux jiffie resolution is 1/100s, so a reused PID starting in the same
// centisecond collides), so an authenticated dashboard user with tight timing
// could SIGTERM an unrelated naozhi-UID process.
//
// pidfd closes the window structurally: pidfd_open(2) pins a *reference* to the
// exact process instance. Once held, that fd never refers to a different
// process even if the original exits and the PID is recycled — pidfd_send_signal(2)
// either signals the original instance or fails with ESRCH (it exited). We
// still re-read start_time *through the pinned identity* as the cross-platform
// contract guard so callers on every OS see the same ErrPidReused semantics,
// but on Linux the signal itself can no longer leak to a recycled PID.
//
// expectedStartTime is the value captured at discovery time (discovery.ProcStartTime,
// /proc/<pid>/stat field 22). startTimeFn reads the *current* start time for the
// pid; it is injected so the caller (which already imports internal/discovery)
// supplies discovery.ProcStartTime without osutil reverse-importing discovery.
//
// Returns:
//   - nil                  — SIGTERM delivered to the verified instance.
//   - ErrPidReused         — identity mismatch; nothing was signalled.
//   - syscall.ESRCH        — process exited; callers treat as success.
//   - other errors         — pidfd_open / send_signal failure.
func SendTermVerified(pid int, expectedStartTime uint64, startTimeFn func(int) (uint64, error)) error {
	if pid <= 0 {
		return syscall.ESRCH
	}

	// Pin the process instance. After this succeeds the fd refers to this
	// exact process; PID recycling cannot alias it. PIDFD_NONBLOCK is
	// irrelevant for signalling, so request 0.
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		// ESRCH: process already gone — treat as success (matches the old
		// PidAlive==false short-circuit). EINVAL/ENOSYS from a kernel without
		// pidfd support is impossible on the AL2023 target (kernel ≥ 5.3) but
		// is surfaced so the caller can fall back rather than silently no-op.
		if errors.Is(err, syscall.ESRCH) {
			return syscall.ESRCH
		}
		return err
	}
	defer unix.Close(pidfd)

	// Identity guard: now that the instance is pinned, confirm its start_time
	// still matches the value captured at discovery. If the original exited
	// before PidfdOpen and the PID was recycled, PidfdOpen already pinned the
	// *new* instance, so this comparison rejects it. expectedStartTime==0
	// means "no expectation recorded" and is rejected by the handler before
	// we get here, but guard defensively.
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
