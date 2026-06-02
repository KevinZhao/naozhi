package cli

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/shim"
)

// TestSpawn_RejectsNULInCwd verifies R164029-SEC-6: Spawn must reject a
// WorkingDir that contains a NUL byte. An OS silently truncates a NUL-embedded
// path at the NUL, which can redirect the working directory to an attacker-
// controlled location. The check fires immediately after the ShimManager
// nil-guard, before any further field dereferences.
func TestSpawn_RejectsNULInCwd(t *testing.T) {
	t.Parallel()
	// Use a zero-value Manager (non-nil) to pass the ShimManager nil guard.
	// The NUL check must fire and return an error before Spawn touches
	// Protocol or any other field.
	w := &Wrapper{
		CLIPath:     "",
		ShimManager: &shim.Manager{},
	}
	_, err := w.Spawn(t.Context(), SpawnOptions{
		Key:        "test",
		WorkingDir: "/tmp/safe\x00/evil",
	})
	if err == nil {
		t.Fatal("Spawn with NUL in WorkingDir should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "NUL") {
		t.Errorf("error should mention NUL, got %q", err.Error())
	}
}

// TestSpawn_NULCheckNotTriggeredByCleanCwd confirms the NUL guard does not
// fire for a path with no NUL bytes. We only verify absence of the NUL error
// message; the call is expected to fail further along (uninitialised Wrapper),
// so we recover from any panic.
func TestSpawn_NULCheckNotTriggeredByCleanCwd(t *testing.T) {
	t.Parallel()
	w := &Wrapper{
		CLIPath:     "",
		ShimManager: &shim.Manager{},
	}
	var err error
	func() {
		defer func() { recover() }() //nolint:errcheck // intentional: we only care that NUL is not the cause
		_, err = w.Spawn(t.Context(), SpawnOptions{
			Key:        "test",
			WorkingDir: "/tmp/legitimate-path",
		})
	}()
	if err != nil && strings.Contains(err.Error(), "NUL") {
		t.Errorf("clean path should not produce a NUL error, got %q", err.Error())
	}
}
