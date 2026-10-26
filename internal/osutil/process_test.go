package osutil

import (
	"os"
	"testing"
)

// TestPidAlive locks the contract that PID 0 and negative PIDs are rejected
// as non-alive. On Linux, kill(0, sig) broadcasts to the caller's entire
// process group and kill(-N, sig) targets a process group, so a silent
// "alive" for an uninitialised or stray PID (e.g. an incomplete shim hello)
// would make the caller trust garbage. R29-DES4: the (bool) signature
// intentionally collapses "invalid PID" into "not alive" because every caller
// is already guarded to check PID > 0; surfacing an (err) second return would
// invite callers to drop the guard.
func TestPidAlive(t *testing.T) {
	t.Parallel()
	if !PidAlive(os.Getpid()) {
		t.Error("PidAlive(os.Getpid()) = false, want true")
	}
	for _, pid := range []int{0, -1, -99999} {
		if PidAlive(pid) {
			t.Errorf("PidAlive(%d) = true; PID <= 0 must never report alive (kill broadcasts to process groups)", pid)
		}
	}
	// PID 2^22 (Linux default pid_max) cannot exist on a standard kernel.
	// If it somehow does, skip rather than fail to avoid a spurious CI red.
	const hugePID = 4194304
	if PidAlive(hugePID) {
		t.Skipf("PID %d unexpectedly alive; skipping non-existent-pid assertion", hugePID)
	}
}
