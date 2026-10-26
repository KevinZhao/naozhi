package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEnforceCLIPathSafe pins the spawn-time cliPath gate. Refused: a
// directory (exec would fail with a less-diagnostic message) and any
// non-absolute path (R20260603040203-SEC-2: exec.Command re-resolves a bare
// name against the live PATH / CWD — a PATH-poisoning vector). Allowed: an
// empty or missing path (the CLI may not be installed yet; the shim reports
// it), a regular file and a symlink to one. FIFO / socket rows live in
// wrapper_enforce_clipath_unix_test.go.
func TestEnforceCLIPathSafe(t *testing.T) {
	writeBin := func(t *testing.T, path string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho v1\n"), 0o755); err != nil {
			t.Fatalf("write bin: %v", err)
		}
		return path
	}
	fixed := func(p string) func(*testing.T) string { return func(*testing.T) string { return p } }
	cases := []struct {
		name      string
		path      func(t *testing.T) string
		errSubstr string // "" = must pass
	}{
		{"directory", func(t *testing.T) string { return t.TempDir() }, "regular file"},
		{"bare name", fixed("claude"), "absolute"},
		{"dot relative", fixed("./claude"), "absolute"},
		{"parent relative", fixed("../bin/evil"), "absolute"},
		{"relative dir", fixed("bin/claude"), "absolute"},
		{"empty", fixed(""), ""},
		{"missing in tempdir", func(t *testing.T) string { return filepath.Join(t.TempDir(), "does-not-exist") }, ""},
		{"absolute missing", fixed("/nonexistent/abs/claude"), ""},
		{"regular file", func(t *testing.T) string { return writeBin(t, filepath.Join(t.TempDir(), "fakecli")) }, ""},
		{"symlink to regular file", func(t *testing.T) string {
			if runtime.GOOS == "windows" {
				t.Skip("symlink semantics differ on windows")
			}
			dir := t.TempDir()
			link := filepath.Join(dir, "link")
			if err := os.Symlink(writeBin(t, filepath.Join(dir, "real")), link); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			return link
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path(t)
			err := enforceCLIPathSafe(path)
			if tc.errSubstr == "" {
				if err != nil {
					t.Fatalf("enforceCLIPathSafe(%q) = %v, want nil", path, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
				t.Fatalf("enforceCLIPathSafe(%q) = %v, want refusal mentioning %q", path, err, tc.errSubstr)
			}
		})
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
