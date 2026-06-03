package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSnapshotKnownIDsSorted_DeterministicAndMemoised pins R220123-PERF-19
// (#1638): snapshotKnownIDsSortedLocked must (a) return the IDs sorted
// ascending (the R180-GO-P2 stable-bytes contract), (b) reuse its cached
// sort when knownIDsGen is unchanged, and (c) rebuild when a new ID bumps the
// gen. We observe the cache reuse by checking the cached backing slice
// identity does not change across repeated snapshots of an unchanged set.
func TestSnapshotKnownIDsSorted_DeterministicAndMemoised(t *testing.T) {
	r := &Router{knownIDs: make(map[string]bool)}
	for _, id := range []string{"ccc", "aaa", "bbb"} {
		r.trackSessionID(id)
	}

	got1 := r.snapshotKnownIDsSortedLocked()
	want := []string{"aaa", "bbb", "ccc"}
	if !slices.Equal(got1, want) {
		t.Fatalf("snapshot = %v, want sorted %v", got1, want)
	}

	// Cache must be built and tagged with the current gen.
	cachePtr1 := r.knownIDsSortedCache
	if r.knownIDsSortedGen != r.knownIDsGen {
		t.Fatalf("cache gen %d != knownIDsGen %d", r.knownIDsSortedGen, r.knownIDsGen)
	}

	// Second snapshot with NO mutation: cache backing slice must be reused
	// (no re-sort / re-alloc of the cache), though the returned slice is a
	// fresh clone.
	got2 := r.snapshotKnownIDsSortedLocked()
	if !slices.Equal(got2, want) {
		t.Errorf("second snapshot = %v, want %v", got2, want)
	}
	if &r.knownIDsSortedCache[0] != &cachePtr1[0] {
		t.Error("cache backing slice was rebuilt despite no mutation (memoisation broken)")
	}
	// Returned slice must be a distinct copy, not the cache itself.
	if len(got2) > 0 && &got2[0] == &r.knownIDsSortedCache[0] {
		t.Error("snapshot aliases the cache slice; concurrent rebuild could corrupt the in-flight save")
	}

	// Track a new ID → gen bumps → next snapshot rebuilds and re-sorts.
	r.trackSessionID("aab")
	got3 := r.snapshotKnownIDsSortedLocked()
	want3 := []string{"aaa", "aab", "bbb", "ccc"}
	if !slices.Equal(got3, want3) {
		t.Errorf("post-mutation snapshot = %v, want %v", got3, want3)
	}
	if r.knownIDsSortedGen != r.knownIDsGen {
		t.Errorf("cache gen not advanced after mutation: %d vs %d", r.knownIDsSortedGen, r.knownIDsGen)
	}
}

// TestSaveLoadKnownIDs_RoundTripSorted verifies saveKnownIDs (now taking a
// pre-sorted slice) round-trips through loadKnownIDs unchanged, so the
// signature change in #1638 preserves the on-disk contract.
func TestSaveLoadKnownIDs_RoundTripSorted(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")

	r := &Router{knownIDs: make(map[string]bool)}
	for _, id := range []string{"zeta", "alpha", "mike"} {
		r.trackSessionID(id)
	}
	sorted := r.snapshotKnownIDsSortedLocked()

	if err := saveKnownIDs(storePath, sorted); err != nil {
		t.Fatalf("saveKnownIDs: %v", err)
	}
	loaded := loadKnownIDs(storePath)
	if loaded == nil {
		t.Fatal("loadKnownIDs returned nil")
	}
	for _, id := range []string{"zeta", "alpha", "mike"} {
		if !loaded[id] {
			t.Errorf("loaded set missing %q", id)
		}
	}
	if len(loaded) != 3 {
		t.Errorf("loaded %d IDs, want 3", len(loaded))
	}
}

// TestSaveKnownIDs_StableBytesAcrossSaves pins the R180-GO-P2 stable-bytes
// goal survives the memoisation: two saves of the same logical set produce
// byte-identical files (the cached sorted order is deterministic).
func TestSaveKnownIDs_StableBytesAcrossSaves(t *testing.T) {
	tmp := t.TempDir()
	p1 := filepath.Join(tmp, "a", "sessions.json")
	p2 := filepath.Join(tmp, "b", "sessions.json")

	r := &Router{knownIDs: make(map[string]bool)}
	for _, id := range []string{"d", "a", "c", "b"} {
		r.trackSessionID(id)
	}

	if err := saveKnownIDs(p1, r.snapshotKnownIDsSortedLocked()); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := saveKnownIDs(p2, r.snapshotKnownIDsSortedLocked()); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	b1, err := os.ReadFile(knownIDsPath(p1))
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	b2, err := os.ReadFile(knownIDsPath(p2))
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Errorf("known-IDs bytes differ across saves of same set:\n %q\n %q", b1, b2)
	}
}
