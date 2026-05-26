package cron

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadJobs_SymlinkRefused verifies that O_NOFOLLOW (R238-SEC-8 / #829)
// closes the prior Lstat→Open TOCTOU window: when cron_jobs.json is a
// symlink, loadJobs must return an error rather than reading the target.
//
// Pre-fix (Lstat-then-Open) the symlink check ran in a separate syscall
// from the Open, leaving a window for an attacker to swap the file. The
// post-fix code uses os.OpenFile(O_RDONLY|O_NOFOLLOW) so the kernel
// performs the symlink reject atomically with the open.
func TestLoadJobs_SymlinkRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "cron_jobs.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	jobs, err := loadJobs(link)
	if err == nil {
		t.Fatalf("expected error for symlink path; got jobs=%v err=nil", jobs)
	}
	if jobs != nil {
		t.Errorf("expected nil jobs map on symlink refusal, got %v", jobs)
	}
	t.Logf("loadJobs symlink err: %v", err)
}

// TestLoadJobs_NotExistReturnsEmpty pins the "fresh deployment" path: a
// missing cron_jobs.json must yield (nil, nil) so Scheduler.Start treats
// it as empty rather than aborting. R238-SEC-8 introduced an OpenFile
// path that must keep this contract intact (errors.Is(err, fs.ErrNotExist)
// must short-circuit before the symlink-error branch).
func TestLoadJobs_NotExistReturnsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "no_such_file.json")
	jobs, err := loadJobs(missing)
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if jobs != nil && len(jobs) != 0 {
		t.Errorf("expected empty jobs map, got %d entries", len(jobs))
	}
}

// TestLoadJobs_RegularFileLoads ensures the happy path still parses a
// regular file end-to-end after the OpenFile/Fstat refactor: regression
// pin against an over-eager Fstat guard rejecting normal files.
func TestLoadJobs_RegularFileLoads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	if err := os.WriteFile(path, []byte(`[{"id":"abc","schedule":"@every 1h","prompt":"x"}]`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	jobs, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 1 || jobs["abc"] == nil {
		t.Errorf("expected one job 'abc', got %v", jobs)
	}
}
