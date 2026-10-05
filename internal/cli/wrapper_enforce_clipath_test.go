package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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

// R20260603040203-SEC-2: a non-absolute cliPath must be REJECTED at spawn
// time. A bare name like "claude" (or "../bin/evil") is re-resolved by
// exec.Command against the live PATH / CWD — a PATH-poisoning argv-injection
// vector. The hard refusal lives here, not in warn-only construction.
func TestEnforceCLIPathSafe_RejectsRelativePath(t *testing.T) {
	for _, rel := range []string{"claude", "./claude", "../bin/evil", "bin/claude"} {
		err := enforceCLIPathSafe(rel)
		if err == nil {
			t.Errorf("enforceCLIPathSafe(%q) returned nil; expected refusal for relative path", rel)
			continue
		}
		if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("error for %q should mention 'absolute', got %q", rel, err.Error())
		}
	}
}

// An absolute path that does not exist must still pass (uninstalled-CLI
// rationale) — the relative-path guard must not regress the ENOENT case.
func TestEnforceCLIPathSafe_AbsoluteMissingStillOK(t *testing.T) {
	if err := enforceCLIPathSafe("/nonexistent/abs/claude"); err != nil {
		t.Fatalf("absolute missing path should not error, got %v", err)
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
