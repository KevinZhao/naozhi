//go:build linux

package shim

import (
	"os"
	"strings"
	"testing"
)

// TestVerifyCLIExeMatch_Self pins R216-SEC-5 (#546): when wantCLIPath
// agrees with /proc/<pid>/exe the helper returns nil so the caller may
// adopt cliPID into the privileged cgroup. We use os.Getpid() + the test
// binary's own /proc/self/exe target as a self-consistent fixture that
// works under `go test` without sudo or fixture binaries.
func TestVerifyCLIExeMatch_Self(t *testing.T) {
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Fatalf("readlink /proc/self/exe: %v", err)
	}
	// Strip the " (deleted)" suffix the same way verifyCLIExeMatch does so
	// the comparison stays apples-to-apples even when `go test` has just
	// rebuilt the binary in-place.
	want := strings.TrimSuffix(exe, " (deleted)")

	got, err := verifyCLIExeMatch(os.Getpid(), want)
	if err != nil {
		t.Fatalf("verifyCLIExeMatch(self, self) returned err = %v, want nil", err)
	}
	if got != want {
		t.Errorf("cleanExe = %q, want %q", got, want)
	}
}

// TestVerifyCLIExeMatch_Mismatch pins the rejection path: a wantCLIPath
// that does NOT match /proc/<pid>/exe returns a non-nil error and the
// resolved exe path so the caller can log + refuse adoption. This is the
// branch that defends against a shim spawning an attacker-influenced
// child whose PPid happens to satisfy the earlier R229-SEC-4 check but
// whose binary is not the configured CLI.
func TestVerifyCLIExeMatch_Mismatch(t *testing.T) {
	got, err := verifyCLIExeMatch(os.Getpid(), "/definitely/not/the/cli/binary")
	if err == nil {
		t.Fatal("verifyCLIExeMatch with mismatched wantCLIPath returned nil err, want error")
	}
	if got == "" {
		t.Errorf("cleanExe = %q, want non-empty so caller can log the actual exe", got)
	}
	// The error must carry both the resolved and configured paths so the
	// log line at the call site has enough context for an operator to
	// chase down the mismatch.
	if !strings.Contains(err.Error(), "/definitely/not/the/cli/binary") {
		t.Errorf("err = %q, want mention of wantCLIPath", err.Error())
	}
}

// TestVerifyCLIExeMatch_NonExistentPID pins the readlink-failed branch:
// a PID that does not exist must surface a non-nil error AND empty
// cleanExe so moveToShimsCgroup distinguishes "skip — cannot validate"
// from "reject — actively wrong binary".
func TestVerifyCLIExeMatch_NonExistentPID(t *testing.T) {
	// PID 2^31-1 is reserved on Linux and never assigned in practice.
	got, err := verifyCLIExeMatch(1<<31-1, "/usr/bin/claude")
	if err == nil {
		t.Fatal("expected error for non-existent PID, got nil")
	}
	if got != "" {
		t.Errorf("cleanExe = %q, want empty so caller selects the skip log branch", got)
	}
}
