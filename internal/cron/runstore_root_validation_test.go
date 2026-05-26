package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNewRunStore_AbsolutisesRelativeStorePath covers issue #825
// (R245-SEC-1): newRunStore must canonicalise the StorePath via
// filepath.Abs before deriving the runs dir, otherwise a relative
// StorePath leaves the runs dir resolved relative to PWD and any later
// chdir would re-route writes/reads.
func TestNewRunStore_AbsolutisesRelativeStorePath(t *testing.T) {
	tmp := t.TempDir()
	// Build a relative path. Use t.Chdir so PWD is deterministic and the
	// abs() result lands inside tmp.
	t.Chdir(tmp)
	rel := "subdir/cron_jobs.json"
	if err := os.MkdirAll(filepath.Join(tmp, "subdir"), 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := newRunStore(rel, 0, 0)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore disabled unexpectedly")
	}
	if !filepath.IsAbs(s.root) {
		t.Errorf("runStore.root = %q, want absolute path", s.root)
	}
	wantPrefix := filepath.Join(tmp, "subdir", "runs")
	if s.root != wantPrefix {
		t.Errorf("runStore.root = %q, want %q (must abs+EvalSymlinks)", s.root, wantPrefix)
	}
}

// TestNewRunStore_RefusesSymlinkedRunsDir covers the symlink validation
// half of #825: a pre-existing symlink at runs/ pointing outside the
// store dir would let writeRun follow the link target.
func TestNewRunStore_RefusesSymlinkedRunsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics; skip on windows")
	}
	tmp := t.TempDir()
	target := t.TempDir()
	storeDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	// Pre-create runs/ as a symlink to a sibling tmp.
	link := filepath.Join(storeDir, "runs")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	s := newRunStore(filepath.Join(storeDir, "cron_jobs.json"), 0, 0)
	if s == nil {
		t.Fatalf("nil runStore")
	}
	if !s.disabled {
		t.Errorf("runStore should be disabled when runs/ is a symlink, got root=%q", s.root)
	}
}

// TestNewRunStore_TightensInheritedLooseRunsDirMode covers the perm
// tighten path: if runs/ pre-exists with 0o755 (umask leak), the new
// constructor chmods it to 0o700 so non-owner users cannot list jobIDs.
func TestNewRunStore_TightensInheritedLooseRunsDirMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics; skip on windows")
	}
	tmp := t.TempDir()
	storeDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	runsPre := filepath.Join(storeDir, "runs")
	if err := os.Mkdir(runsPre, 0o755); err != nil {
		t.Fatalf("seed loose runs/: %v", err)
	}

	s := newRunStore(filepath.Join(storeDir, "cron_jobs.json"), 0, 0)
	if s == nil || s.disabled {
		t.Fatalf("runStore disabled unexpectedly: %+v", s)
	}
	fi, err := os.Stat(runsPre)
	if err != nil {
		t.Fatalf("stat runs: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("runs dir perm = %#o, want 0o700", got)
	}
}
