package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/session/knownids"
)

// TestSaveLoadKnownIDs_RoundTripSorted verifies saveKnownIDs (taking the
// pre-sorted SortedSnapshot) round-trips through loadKnownIDs unchanged.
func TestSaveLoadKnownIDs_RoundTripSorted(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")

	var kid knownids.Store
	for _, id := range []string{"zeta", "alpha", "mike"} {
		kid.Track(id)
	}
	sorted := kid.SortedSnapshot()

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

// TestSaveKnownIDs_StableBytesAcrossSaves pins the stable-bytes contract: two
// saves of the same logical set produce byte-identical files.
func TestSaveKnownIDs_StableBytesAcrossSaves(t *testing.T) {
	tmp := t.TempDir()
	p1 := filepath.Join(tmp, "a", "sessions.json")
	p2 := filepath.Join(tmp, "b", "sessions.json")

	var kid knownids.Store
	for _, id := range []string{"d", "a", "c", "b"} {
		kid.Track(id)
	}

	if err := saveKnownIDs(p1, kid.SortedSnapshot()); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := saveKnownIDs(p2, kid.SortedSnapshot()); err != nil {
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
