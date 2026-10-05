//go:build !windows

package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// R20260527122801-SEC-1: enforceCLIPathSafe must REJECT FIFO / socket /
// directory cliPath at spawn time, even though construction-time
// validateCLIPath is warn-only. The construction-time check is the audit
// trail; this is the last-hop refusal before exec.Command sees the
// argv. Ensures a `cli.path = /tmp/fifo` misconfig cannot deliver a
// file-type-confusion attack to the shim.
func TestEnforceCLIPathSafe_RejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "fakecli.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	defer os.Remove(fifoPath)

	err := enforceCLIPathSafe(fifoPath)
	if err == nil {
		t.Fatalf("enforceCLIPathSafe(%q) returned nil; expected refusal for FIFO", fifoPath)
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("error message should mention file-type rejection, got %q", err.Error())
	}
}

// Socket should also be refused — same file-type-confusion class as FIFO.
func TestEnforceCLIPathSafe_RejectsSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "s.sock")
	// macOS sun_path cap is ~104 bytes — skip if the temp path is too long.
	if len(sockPath) >= 100 {
		t.Skipf("socket path too long for sun_path: %d", len(sockPath))
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("unix listen unavailable: %v", err)
	}
	defer l.Close()
	defer os.Remove(sockPath)

	if err := enforceCLIPathSafe(sockPath); err == nil {
		t.Fatalf("enforceCLIPathSafe(%q) returned nil; expected refusal for socket", sockPath)
	}
}
