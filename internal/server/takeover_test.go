package server

import (
	"os"
	"runtime"
	"testing"
)

// TestVerifyProcOwnedByEuid_Self confirms that the helper accepts a process
// that runs under the current effective UID (the test process itself).
// R20260526-SEC-009.
func TestVerifyProcOwnedByEuid_Self(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("UID check is Linux-only (uses /proc)")
	}
	if err := verifyProcOwnedByEuid(os.Getpid()); err != nil {
		t.Errorf("verifyProcOwnedByEuid(self) = %v, want nil", err)
	}
}

// TestVerifyProcOwnedByEuid_Init confirms the helper rejects PID 1 when the
// test runs as a non-root user (PID 1 is owned by root). When the test runs
// as root (e.g. inside some CI containers), euid==0 matches and we skip.
// R20260526-SEC-009.
func TestVerifyProcOwnedByEuid_Init(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("UID check is Linux-only (uses /proc)")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: euid matches PID 1's UID, can't exercise mismatch path")
	}
	if _, err := os.Stat("/proc/1"); err != nil {
		t.Skipf("/proc/1 unavailable: %v", err)
	}
	err := verifyProcOwnedByEuid(1)
	if err == nil {
		t.Error("verifyProcOwnedByEuid(1) returned nil; expected mismatch error for root-owned PID 1")
	}
}

// TestVerifyProcOwnedByEuid_NonLinux is a placeholder: on non-Linux platforms
// the helper is a no-op and must not return an error for any PID.
func TestVerifyProcOwnedByEuid_NonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux-specific path covered by other tests")
	}
	if err := verifyProcOwnedByEuid(os.Getpid()); err != nil {
		t.Errorf("verifyProcOwnedByEuid on non-linux should be no-op, got %v", err)
	}
}
