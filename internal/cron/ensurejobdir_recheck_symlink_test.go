//go:build !windows

package cron

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureJobDir_RechecksSymlinkAfterCache pins R20260608133928-CR-4 (#1968):
// the per-job symlink guard must run on EVERY ensureJobDir call, not only the
// first. Previously the function returned early on a jobDirEnsured cache hit,
// so an attacker who swapped runs/<jobID>/ for a symlink AFTER the first Append
// (cache already populated) would bypass the Lstat check on every subsequent
// cron tick and have run records land at the symlink target.
//
// The test populates the cache via a legitimate first call, then replaces the
// real dir with a symlink pointing outside the runs root and asserts the next
// call is still rejected and leaves the redirect target empty.
func TestEnsureJobDir_RechecksSymlinkAfterCache(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 0)

	jobID := "00112233aabbccdd"
	dir := filepath.Join(s.root, jobID)

	// 1. Legitimate first call: creates the dir and warms the cache.
	if err := s.ensureJobDir(jobID, dir); err != nil {
		t.Fatalf("first ensureJobDir: %v", err)
	}
	if _, ok := s.jobDirEnsured.Load(jobID); !ok {
		t.Fatal("cache not warmed after first ensureJobDir")
	}

	// 2. Attacker swaps the real dir for a symlink to an outside target,
	//    AFTER the cache is populated.
	evil := t.TempDir()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("rm real dir: %v", err)
	}
	if err := os.Symlink(evil, dir); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	// 3. Next call MUST re-Lstat and reject despite the warm cache.
	if err := s.ensureJobDir(jobID, dir); err == nil {
		t.Fatal("ensureJobDir must reject a post-cache symlink swap (#1968)")
	}

	// The redirect target must stay empty and the stale cache entry dropped.
	entries, rerr := os.ReadDir(evil)
	if rerr != nil {
		t.Fatalf("read evil dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target was written through: %d entries", len(entries))
	}
	if _, ok := s.jobDirEnsured.Load(jobID); ok {
		t.Fatal("stale cache entry must be dropped after rejecting a symlink swap")
	}
}
