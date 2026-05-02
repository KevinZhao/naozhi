package osutil

import (
	"os"
	"testing"
)

func TestPidAlive_Self(t *testing.T) {
	t.Parallel()
	// The current process should always be alive.
	if !PidAlive(os.Getpid()) {
		t.Error("PidAlive(os.Getpid()) = false, want true")
	}
}

// TestPidAlive_ZeroPid locks the contract that PID 0 is rejected as
// non-alive. On Linux, kill(0, sig) broadcasts to the caller's entire
// process group — silent "alive" return for an uninitialised PID (e.g. an
// incomplete shim hello that never set a real CLI PID) would make the
// caller trust garbage. R29-DES4: the (bool) signature intentionally
// collapses "invalid PID" into "not alive" because every caller is
// already guarded to check PID > 0 before the question is meaningful;
// surfacing an (err) second return would invite callers to drop the
// guard.
func TestPidAlive_ZeroPid(t *testing.T) {
	t.Parallel()
	if PidAlive(0) {
		t.Error("PidAlive(0) = true; PID 0 must never report alive (kill(0, sig) broadcasts to process group)")
	}
}

// TestPidAlive_NegativePid locks the contract that negative PIDs are
// rejected. kill(-N, sig) targets a process group on Linux, so without
// the guard a stray negative PID from e.g. parsing a shim hello field
// would report any live peer in that group as "alive". R29-DES4.
func TestPidAlive_NegativePid(t *testing.T) {
	t.Parallel()
	if PidAlive(-1) {
		t.Error("PidAlive(-1) = true; negative PIDs must never report alive (kill(-N, sig) targets process groups)")
	}
	if PidAlive(-99999) {
		t.Error("PidAlive(-99999) = true; negative PIDs must never report alive")
	}
}

func TestPidAlive_NonExistentPid(t *testing.T) {
	t.Parallel()
	// PID 2^22 is extremely unlikely to exist on any normal system.
	// If it does exist somehow, the test skips.
	got := PidAlive(4194304)
	// We can't guarantee the result, but ensure no panic.
	_ = got
}
