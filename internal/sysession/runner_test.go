package sysession

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewRunner_BinPathValidation pins R247-SEC-19: an absolute BinPath that
// is not a regular executable file MUST be rejected at construction time
// rather than degrading at first Tick. A relative BinPath that resolveBin
// PathFromEnv could not lift to absolute is allowed through unchanged so
// the documented "degrade gracefully" path stays intact.
func TestNewRunner_BinPathValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	regularNonExec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(regularNonExec, []byte("not exec"), 0o644); err != nil {
		t.Fatalf("write noexec: %v", err)
	}
	regularExec := filepath.Join(dir, "claude-fake")
	if err := os.WriteFile(regularExec, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	dirAsBin := filepath.Join(dir, "binAsDir")
	if err := os.MkdirAll(dirAsBin, 0o755); err != nil {
		t.Fatalf("mkdir binAsDir: %v", err)
	}

	t.Run("regular_executable_passes", func(t *testing.T) {
		t.Parallel()
		_, err := NewRunner(RunnerConfig{BinPath: regularExec, WorkDir: work})
		if err != nil {
			t.Fatalf("NewRunner with valid abs binary should succeed, got %v", err)
		}
	})

	t.Run("absolute_missing_rejected", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(dir, "does-not-exist")
		_, err := NewRunner(RunnerConfig{BinPath: missing, WorkDir: work})
		if err == nil {
			t.Fatal("NewRunner with missing abs path should error")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("want errors.Is os.ErrNotExist, got %v", err)
		}
	})

	t.Run("absolute_dir_rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewRunner(RunnerConfig{BinPath: dirAsBin, WorkDir: work})
		if err == nil {
			t.Fatal("NewRunner with directory BinPath should error")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("want regular-file error, got %v", err)
		}
	})

	t.Run("absolute_nonexec_rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewRunner(RunnerConfig{BinPath: regularNonExec, WorkDir: work})
		if err == nil {
			t.Fatal("NewRunner with non-executable BinPath should error")
		}
		if !strings.Contains(err.Error(), "not executable") {
			t.Errorf("want executable error, got %v", err)
		}
	})

	t.Run("unresolved_relative_passes", func(t *testing.T) {
		t.Parallel()
		// Stub osStat so resolveBinPathFromEnv finds nothing in PATH and
		// cfg.BinPath stays as a relative literal — the IsAbs guard then
		// skips the validation block, matching the documented "degrade
		// gracefully" path.
		prev := osStat
		osStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
		t.Cleanup(func() { osStat = prev })

		_, err := NewRunner(RunnerConfig{BinPath: "claude-not-installed", WorkDir: work})
		if err != nil {
			t.Fatalf("relative BinPath should still construct (lazy resolve), got %v", err)
		}
	})
}
