package cron

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSchedulerConfig_resolveAllowedRoot_EmptyShortCircuit pins R241-ARCH-10
// (#517): the helper extracted from NewScheduler must short-circuit on the
// empty-root case so EvalSymlinks is not invoked. This keeps the constructor
// hot path free of stray syscalls when AllowedRoot is unset (test fixtures
// + legacy deployments).
func TestSchedulerConfig_resolveAllowedRoot_EmptyShortCircuit(t *testing.T) {
	t.Parallel()
	cfg := &SchedulerConfig{}
	if got := cfg.resolveAllowedRoot(); got != "" {
		t.Fatalf("empty AllowedRoot: got=%q want=empty", got)
	}
}

// TestSchedulerConfig_resolveAllowedRoot_NULPruned pins R241-ARCH-10 (#517) /
// R20260527-PERF-14 (#1297) interaction: a NUL-bearing root is cleared in
// place AND the returned resolved path is empty. Two-step protection so the
// caller's cache-key suffix concatenation cannot embed an attacker-influenced
// NUL even if the caller forgets to re-read cfg.AllowedRoot.
func TestSchedulerConfig_resolveAllowedRoot_NULPruned(t *testing.T) {
	t.Parallel()
	cfg := &SchedulerConfig{AllowedRoot: "/tmp/safe\x00/etc"}
	got := cfg.resolveAllowedRoot()
	if cfg.AllowedRoot != "" {
		t.Errorf("AllowedRoot post-call = %q, want cleared", cfg.AllowedRoot)
	}
	if got != "" {
		t.Errorf("resolved = %q, want empty for NUL-bearing root", got)
	}
}

// TestSchedulerConfig_resolveAllowedRoot_EvalSymlinks pins R241-ARCH-10
// (#517): a real path resolves through EvalSymlinks. The helper must
// preserve the symlink-evaluating semantics that workDirResolveUnderRoot
// relies on for TOCTOU protection.
func TestSchedulerConfig_resolveAllowedRoot_EvalSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &SchedulerConfig{AllowedRoot: root}
	got := cfg.resolveAllowedRoot()
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	if got != want {
		t.Fatalf("resolved = %q, want EvalSymlinks(%q) = %q", got, root, want)
	}
}

// TestSchedulerConfig_resolveAllowedRoot_NonExistent pins R241-ARCH-10
// (#517): a non-existent root resolves to empty (EvalSymlinks errors).
// The caller then leaves allowedRootResolved as empty, and
// workDirResolveUnderRoot's empty-root branch falls back to lazy per-call
// resolution. The original NewScheduler behaviour must survive the
// extraction.
func TestSchedulerConfig_resolveAllowedRoot_NonExistent(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Skipf("expected %q to be missing, got err=%v", missing, err)
	}
	cfg := &SchedulerConfig{AllowedRoot: missing}
	if got := cfg.resolveAllowedRoot(); got != "" {
		t.Fatalf("non-existent root: got=%q want=empty (EvalSymlinks error path)", got)
	}
	if cfg.AllowedRoot != missing {
		t.Errorf("AllowedRoot post-call = %q, want preserved %q (only NUL clears)", cfg.AllowedRoot, missing)
	}
}
