package session

import (
	"path/filepath"
	"testing"
)

// TestSaveKnownIDs_EnsureDirCachedPerDir pins R20260614-PERF-001: saveKnownIDs
// must gate datadir.EnsureDir through the package-level storeDirEnsured cache
// (the same one writeStoreData uses) so steady-state save ticks skip the
// MkdirAll+Lstat+Chmod syscalls on a directory already known to exist.
//
// We observe the gating two ways:
//  1. After the first save, the directory is recorded in storeDirEnsured.
//  2. With the directory already cached (e.g. a prior store save into the same
//     dir), saveKnownIDs leaves the cache entry intact and still succeeds —
//     i.e. it consults the shared cache rather than maintaining its own.
func TestSaveKnownIDs_EnsureDirCachedPerDir(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "store")
	storePath := filepath.Join(dir, "sessions.json")

	if _, cached := storeDirEnsured.Load(dir); cached {
		t.Fatalf("precondition: dir %q already cached in a fresh temp dir", dir)
	}

	// First save into a brand-new directory must EnsureDir and record the dir.
	if err := saveKnownIDs(storePath, []string{"a", "b"}); err != nil {
		t.Fatalf("first saveKnownIDs: %v", err)
	}
	if _, cached := storeDirEnsured.Load(dir); !cached {
		t.Fatalf("dir %q not recorded in storeDirEnsured after first save", dir)
	}

	// Second save into the same directory: the cache is already populated, so
	// EnsureDir is skipped. The save must still succeed and the cache entry
	// must remain. (Correctness of skipping is covered by the round-trip below.)
	if err := saveKnownIDs(storePath, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("second saveKnownIDs: %v", err)
	}
	if _, cached := storeDirEnsured.Load(dir); !cached {
		t.Fatalf("dir %q dropped from storeDirEnsured after second save", dir)
	}

	// The skipped EnsureDir must not corrupt the on-disk contract: the latest
	// write is still durable and loadable.
	loaded := loadKnownIDs(storePath)
	for _, id := range []string{"a", "b", "c"} {
		if !loaded[id] {
			t.Errorf("loaded set missing %q after cached-dir save", id)
		}
	}
}

// TestSaveKnownIDs_SharesEnsureDirCacheWithStore pins that knownIDs and the
// main store share one storeDirEnsured entry per directory: once writeStoreData
// has hardened the dir, saveKnownIDs into the same dir reuses that cache entry
// (it does not need its own EnsureDir pass), and vice versa.
func TestSaveKnownIDs_SharesEnsureDirCacheWithStore(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "shared")
	storePath := filepath.Join(dir, "sessions.json")

	if _, cached := storeDirEnsured.Load(dir); cached {
		t.Fatalf("precondition: dir %q already cached", dir)
	}

	// Prime the cache via the main store write path.
	if err := writeStoreData(storePath, []byte("{}")); err != nil {
		t.Fatalf("writeStoreData: %v", err)
	}
	if _, cached := storeDirEnsured.Load(dir); !cached {
		t.Fatalf("writeStoreData did not record dir %q", dir)
	}

	// saveKnownIDs into the same dir must succeed using the shared cache entry.
	if err := saveKnownIDs(storePath, []string{"x"}); err != nil {
		t.Fatalf("saveKnownIDs after store prime: %v", err)
	}
	if _, cached := storeDirEnsured.Load(dir); !cached {
		t.Fatalf("shared cache entry for %q lost after saveKnownIDs", dir)
	}
}
