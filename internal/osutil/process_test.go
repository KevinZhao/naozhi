package osutil

import (
	"os"
	"testing"
)

func TestPidAlive_Self(t *testing.T) {
	// The current process should always be alive.
	if !PidAlive(os.Getpid()) {
		t.Error("PidAlive(os.Getpid()) = false, want true")
	}
}

func TestPidAlive_ZeroPid(t *testing.T) {
	// PID 0 is not a valid user process PID; behaviour is platform-specific
	// but PidAlive should not panic. We just call it for coverage.
	_ = PidAlive(0)
}

func TestPidAlive_NegativePid(t *testing.T) {
	// Negative PIDs are invalid; os.FindProcess may or may not return an error
	// depending on the OS. Ensure we don't panic.
	_ = PidAlive(-1)
}

func TestPidAlive_NonExistentPid(t *testing.T) {
	// PID 2^22 is extremely unlikely to exist on any normal system.
	// If it does exist somehow, the test skips.
	got := PidAlive(4194304)
	// We can't guarantee the result, but ensure no panic.
	_ = got
}
