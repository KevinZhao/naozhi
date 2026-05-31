//go:build !windows

package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureJobDir_RejectsSymlink pins R20260531A-SEC-2 (#1504): ensureJobDir
// must refuse to create/use a job directory when the target path is already a
// symlink. Without the guard, an attacker who can pre-plant a symlink at
// runs/<jobID>/ can redirect all run records for that job to an arbitrary path
// on the filesystem.
//
// The test plants a symlink at the expected job-dir path before calling
// ensureJobDir; the call must return a non-nil error mentioning "symlink" and
// must NOT populate the jobDirEnsured cache (so a retry after the symlink is
// removed can succeed).
func TestEnsureJobDir_RejectsSymlink(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 0)

	jobID := "deadbeefdeadbeef"
	jobDir := filepath.Join(s.root, jobID)

	// Plant a symlink where the job dir would normally be created.
	target := t.TempDir()
	if err := os.Symlink(target, jobDir); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}

	err := s.ensureJobDir(jobID, jobDir)
	if err == nil {
		t.Fatal("ensureJobDir must return error when job dir is a symlink")
	}
	if !strings.Contains(err.Error(), "not a plain directory") {
		t.Errorf("expected error to mention non-plain-directory rejection, got: %v", err)
	}

	// Cache must NOT be populated — a subsequent call after the symlink is
	// removed must be able to succeed.
	if _, ok := s.jobDirEnsured.Load(jobID); ok {
		t.Fatal("jobDirEnsured cache must not be populated when ensureJobDir rejects a symlink")
	}
}

// TestEnsureJobDir_AcceptsExistingRealDir confirms the Lstat guard does not
// reject a legitimately pre-existing real directory (e.g. created by a prior
// run). The fast-path cache is not yet warm; the Lstat fires and sees a regular
// directory, so MkdirAll proceeds (no-op on existing dir) and the cache is
// populated.
func TestEnsureJobDir_AcceptsExistingRealDir(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 0)

	jobID := "cafebabecafebabe"
	jobDir := filepath.Join(s.root, jobID)

	// Pre-create the directory as a real dir (simulates prior run).
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatalf("pre-create dir: %v", err)
	}

	if err := s.ensureJobDir(jobID, jobDir); err != nil {
		t.Fatalf("ensureJobDir rejected a real pre-existing directory: %v", err)
	}
	if _, ok := s.jobDirEnsured.Load(jobID); !ok {
		t.Fatal("jobDirEnsured cache not populated after successful ensureJobDir on real dir")
	}
}
