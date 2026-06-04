package osutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestSendTermVerified_RejectsNonPositivePID locks the contract that a
// non-positive PID returns ESRCH (treated as success / nothing-to-kill) and
// never reaches the signal syscall — guarding against kill(0)/kill(-N)
// process-group broadcast. (#1670)
func TestSendTermVerified_RejectsNonPositivePID(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1, -99999} {
		if err := SendTermVerified(pid, 1, nil); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("SendTermVerified(%d) = %v, want ESRCH", pid, err)
		}
	}
}

// TestSendTermVerified_DeadPIDReturnsESRCH verifies that a PID with no live
// process resolves to ESRCH (success path for callers) rather than signalling
// an unrelated process. (#1670)
func TestSendTermVerified_DeadPIDReturnsESRCH(t *testing.T) {
	t.Parallel()
	const deadPID = 2147480000 // ~2^31, effectively never live
	err := SendTermVerified(deadPID, 12345, func(int) (uint64, error) {
		return 0, errors.New("should not be consulted for a dead pid")
	})
	if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("SendTermVerified(deadPID) = %v, want ESRCH", err)
	}
}

// TestSendTermVerified_IdentityMismatchDoesNotKill is the core PID-reuse
// guard: a live process whose start_time no longer matches the expectation
// (simulating PID recycling) must return ErrPidReused and must NOT be
// terminated. We use a real long-lived child as the "innocent bystander" and
// feed a startTimeFn that always reports a mismatch. (#1670)
func TestSendTermVerified_IdentityMismatchDoesNotKill(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	pid := cmd.Process.Pid

	// expectedStartTime != actual -> mismatch -> must refuse to signal.
	err := SendTermVerified(pid, 999999, func(int) (uint64, error) {
		return 111111, nil // reported actual != expected
	})
	if !errors.Is(err, ErrPidReused) {
		t.Fatalf("SendTermVerified with identity mismatch = %v, want ErrPidReused", err)
	}

	// The bystander must still be alive — the guard must not have killed it.
	time.Sleep(50 * time.Millisecond)
	if !PidAlive(pid) {
		t.Fatal("identity mismatch killed the process; PID-reuse guard failed")
	}
}

// TestSendTermVerified_MatchingIdentityKills verifies the happy path: a live
// process whose start_time matches the expectation receives SIGTERM and exits.
// (#1670)
func TestSendTermVerified_MatchingIdentityKills(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// startTimeFn echoes the expected value -> identity matches -> signal.
	const expected = uint64(42)
	if err := SendTermVerified(pid, expected, func(int) (uint64, error) {
		return expected, nil
	}); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("SendTermVerified (matching) = %v, want nil/ESRCH", err)
	}

	// Reap and confirm it was signalled (terminated, not still running).
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case <-waitErr:
		// exited (via SIGTERM) — success
	case <-time.After(3 * time.Second):
		t.Fatal("process not terminated by SendTermVerified within 3s")
	}
}

// TestSendTermVerified_StartTimeReadFailureIsConservative verifies that when
// the start_time read fails for a live PID, the primitive refuses to signal
// (ErrPidReused) rather than killing an unverifiable process. (#1670)
func TestSendTermVerified_StartTimeReadFailureIsConservative(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	err := SendTermVerified(pid, 5, func(int) (uint64, error) {
		return 0, os.ErrNotExist
	})
	if !errors.Is(err, ErrPidReused) {
		t.Fatalf("start_time read failure = %v, want ErrPidReused", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !PidAlive(pid) {
		t.Fatal("start_time read failure killed the process; should be conservative")
	}
}
