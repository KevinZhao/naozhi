package cron

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureJobDir_CreatesDurableSubdirAndCachesFastPath pins R249-ARCH-10
// (#976): the first ensureJobDir call for a jobID must create the
// runs/<jobID>/ subdirectory and fsync the runs/ root so the new directory
// entry is durable on crash (WriteFileAtomic only fsyncs the file's immediate
// parent, never the grandparent that holds the freshly-created subdir entry).
// Subsequent calls must hit the jobDirEnsured cache fast-path and skip the
// syscall (and therefore the root fsync) entirely.
//
// We cannot directly observe the fsync syscall portably, so this test pins the
// observable contract: the subdir exists after the first call, and the second
// call short-circuits via the cache (so the steady-state Append path stays
// syscall-free per the godoc).
func TestEnsureJobDir_CreatesDurableSubdirAndCachesFastPath(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 0)

	jobID := "0123456789abcdef"
	dir := filepath.Join(s.root, jobID)

	if err := s.ensureJobDir(jobID, dir); err != nil {
		t.Fatalf("first ensureJobDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat created subdir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", dir)
	}

	// Cache must now be warm so the fast-path returns without re-running
	// MkdirAll + root fsync.
	if _, ok := s.jobDirEnsured.Load(jobID); !ok {
		t.Fatal("jobDirEnsured cache not populated after first ensureJobDir")
	}

	// Second call must be a no-op (cache hit). Remove the dir underneath to
	// prove the fast-path does not re-stat / re-create: if it took the slow
	// path it would recreate the dir; the cache-hit path leaves it absent.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("rm subdir: %v", err)
	}
	if err := s.ensureJobDir(jobID, dir); err != nil {
		t.Fatalf("second ensureJobDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cache fast-path unexpectedly recreated subdir (stat err=%v); "+
			"the second call must short-circuit on jobDirEnsured", err)
	}
}

// TestEnsureJobDir_RetriesAfterFailure confirms the cache is NOT poisoned by a
// transient MkdirAll failure: when the root path is a regular file (so MkdirAll
// of a child fails), ensureJobDir returns the error and leaves the cache empty
// so a later call can retry. This guards the #976 fsync addition against
// accidentally caching a half-created state.
func TestEnsureJobDir_RetriesAfterFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, 0)

	jobID := "abcdef0123456789"
	// Place a regular file where the subdir's parent component must be a dir,
	// forcing MkdirAll to fail with ENOTDIR.
	badParent := filepath.Join(s.root, jobID)
	if err := os.WriteFile(badParent, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}
	childDir := filepath.Join(badParent, "sub")

	if err := s.ensureJobDir(jobID, childDir); err == nil {
		t.Fatal("expected ensureJobDir to fail when parent is a regular file")
	}
	if _, ok := s.jobDirEnsured.Load(jobID); ok {
		t.Fatal("cache must not be populated after a MkdirAll failure (would block retry)")
	}
}
