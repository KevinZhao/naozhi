package cli

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS == "windows" {
		t.Skip("FIFO via mkfifo is POSIX-only")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("AF_UNIX socket as filesystem entry is POSIX-only")
	}
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

// Directories must be refused too: a misconfigured cli.path pointing at
// a directory would make exec.Command fail with a less-diagnostic
// message and (under some kernels) may even attempt argv0 munging.
func TestEnforceCLIPathSafe_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := enforceCLIPathSafe(dir); err == nil {
		t.Fatalf("enforceCLIPathSafe(dir) returned nil; expected refusal for directory")
	}
}

// Empty path must NOT error — operator may legitimately run before
// installing the CLI; spawn-time error surfaces from the shim.
func TestEnforceCLIPathSafe_EmptyOK(t *testing.T) {
	if err := enforceCLIPathSafe(""); err != nil {
		t.Fatalf("empty path should not error, got %v", err)
	}
}

// ENOENT must NOT error — same uninstalled-CLI rationale; downstream
// shim spawn produces the operator-facing message.
func TestEnforceCLIPathSafe_MissingFileOK(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	if err := enforceCLIPathSafe(missing); err != nil {
		t.Fatalf("missing path should not error, got %v", err)
	}
}

// Regular executable file must pass through cleanly.
func TestEnforceCLIPathSafe_RegularFileOK(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakecli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho v1\n"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	if err := enforceCLIPathSafe(bin); err != nil {
		t.Fatalf("regular file should pass, got %v", err)
	}
}

// Symlink to a regular file must pass through cleanly.
func TestEnforceCLIPathSafe_SymlinkOK(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := enforceCLIPathSafe(link); err != nil {
		t.Fatalf("symlink should pass, got %v", err)
	}
}

// Spawn ordering: the existing nil-ShimManager early-return MUST still
// fire before our new enforcement, so existing test fixtures that
// construct Wrapper{} with empty CLIPath + nil ShimManager keep their
// existing diagnostic failure mode.
func TestSpawn_OrderingShimManagerFirst(t *testing.T) {
	w := &Wrapper{CLIPath: "/dev/null"} // char-device → would be unsafe
	_, err := w.Spawn(t.Context(), SpawnOptions{Key: "test"})
	if err == nil {
		t.Fatal("expected error for nil ShimManager")
	}
	if !strings.Contains(err.Error(), "shim manager not configured") {
		t.Errorf("expected shim-manager error, got %v", err)
	}
}
