//go:build linux

package shim

import (
	"os"
	"testing"
)

// TestReadPPidFromProcStatus_Self pins R229-SEC-4: readPPidFromProcStatus
// must return the parent PID for a process whose /proc/<pid>/status is
// readable. Use os.Getpid() and verify the returned PPid matches
// os.Getppid() — these are guaranteed to agree on a healthy Linux system.
func TestReadPPidFromProcStatus_Self(t *testing.T) {
	got, err := readPPidFromProcStatus(os.Getpid())
	if err != nil {
		t.Fatalf("readPPidFromProcStatus(self): %v", err)
	}
	want := os.Getppid()
	if got != want {
		t.Errorf("PPid = %d, want %d (os.Getppid)", got, want)
	}
}

// TestReadPPidFromProcStatus_NonExistentPID pins error semantics: a PID
// that does not exist must surface a non-nil error so moveToShimsCgroup
// can refuse to adopt the unverified value.
func TestReadPPidFromProcStatus_NonExistentPID(t *testing.T) {
	// PID 2^31-1 is reserved on Linux and never assigned in practice.
	_, err := readPPidFromProcStatus(1<<31 - 1)
	if err == nil {
		t.Fatal("expected error for non-existent PID, got nil")
	}
}
