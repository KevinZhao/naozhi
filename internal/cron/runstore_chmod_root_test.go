package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRunStore_ChmodsExistingRunsRootTo0700 pins R247-SEC-12 (#504): when
// runs/ already exists with a too-permissive mode (0o755 / 0o777), the
// runStore constructor must Chmod it down to 0o700 on next startup.
// MkdirAll honours `perm` only on directories it creates, so a pre-
// existing runs/ tree (laid down by a prior version or by an attacker
// who racd ahead) keeps whatever mode it had — leaving cron run JSON
// files (which carry script source, env values, stdout summaries) world-
// readable. The chmod is the perm-tightening step that closes that gap.
func TestRunStore_ChmodsExistingRunsRootTo0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix mode bits not enforced on windows")
	}
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	// Pre-create runs/ with 0o755 — simulates an upgrade from a version
	// that didn't tighten the leaf mode, OR an operator who chmod-ed the
	// dir manually and left it world-readable.
	root := filepath.Join(tmp, "runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("seed runs root: %v", err)
	}
	// Some umasks mask out the group/other bits. Force 0o755 explicitly so
	// the test starts from the "too permissive" state regardless of umask.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod runs root to 0755: %v", err)
	}

	s := newRunStore(storePath, 10, time.Hour)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore returned nil/disabled despite valid root")
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat runs root post-construct: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("runs root mode = %#o after newRunStore, want 0o700 (R247-SEC-12 #504 regression)", perm)
	}
}

// TestRunStore_PreservesAlready0700RunsRoot pins the no-op path: when
// runs/ is already at 0o700 the constructor leaves it alone (no chmod
// log noise, no syscall surprise). Verified via stat round-trip — if a
// future change accidentally chmod's on every startup, the test still
// passes because the mode is unchanged, but the godoc here documents
// the intent so a reviewer adding a logging hook to chmod sees the
// expectation.
func TestRunStore_PreservesAlready0700RunsRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix mode bits not enforced on windows")
	}
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	root := filepath.Join(tmp, "runs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("seed runs root: %v", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod runs root: %v", err)
	}
	s := newRunStore(storePath, 10, time.Hour)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore returned nil/disabled despite valid root")
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat runs root: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("runs root mode = %#o, want 0o700 (constructor must not weaken existing mode)", perm)
	}
}
