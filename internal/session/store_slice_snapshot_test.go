package session

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestSaveStoreSlice_ParityWithMap pins R20260602190132-PERF-4: the slice-fed
// save path used by the hot Cleanup / saveIfDirty snapshots must produce a
// store byte-identical to the map-fed saveStore for the same set of sessions.
// We compare by re-parsing both into a map keyed by Key so iteration order
// doesn't make the test flaky.
func TestSaveStoreSlice_ParityWithMap(t *testing.T) {
	a := newSessionWithID("feishu:direct:alice:general", "sess-1")
	a.SetUserLabel("alpha")
	b := newSessionWithID("feishu:group:room:general", "sess-2")

	sessionsMap := map[string]*ManagedSession{a.key: a, b.key: b}
	sessionsSlice := []*ManagedSession{a, b}

	dir := t.TempDir()
	mapPath := filepath.Join(dir, "map", "sessions.json")
	slicePath := filepath.Join(dir, "slice", "sessions.json")

	if err := saveStore(mapPath, sessionsMap); err != nil {
		t.Fatalf("saveStore(map) error: %v", err)
	}
	if err := saveStoreSlice(slicePath, sessionsSlice); err != nil {
		t.Fatalf("saveStoreSlice error: %v", err)
	}

	mapEntries := loadStore(mapPath)
	sliceEntries := loadStore(slicePath)
	if len(mapEntries) != 2 || len(sliceEntries) != 2 {
		t.Fatalf("loaded entries map=%d slice=%d, want 2 each", len(mapEntries), len(sliceEntries))
	}
	for key, me := range mapEntries {
		se, ok := sliceEntries[key]
		if !ok {
			t.Fatalf("slice store missing key %q", key)
		}
		mb, _ := json.Marshal(me)
		sb, _ := json.Marshal(se)
		if string(mb) != string(sb) {
			t.Errorf("entry %q differs:\nmap=%s\nslice=%s", key, mb, sb)
		}
	}
}

// TestSaveKnownIDsSlice_ParityWithMap pins that saveKnownIDsSlice produces the
// same sorted on-disk set as the map-fed saveKnownIDs (R20260602190132-PERF-4).
func TestSaveKnownIDsSlice_ParityWithMap(t *testing.T) {
	ids := map[string]bool{"zeta": true, "alpha": true, "mike": true}
	slice := []string{"mike", "zeta", "alpha"} // intentionally unsorted

	dir := t.TempDir()
	mapPath := filepath.Join(dir, "map", "sessions.json")
	slicePath := filepath.Join(dir, "slice", "sessions.json")

	if err := saveKnownIDs(mapPath, ids); err != nil {
		t.Fatalf("saveKnownIDs(map) error: %v", err)
	}
	if err := saveKnownIDsSlice(slicePath, slice); err != nil {
		t.Fatalf("saveKnownIDsSlice error: %v", err)
	}

	mapLoaded := loadKnownIDs(mapPath)
	sliceLoaded := loadKnownIDs(slicePath)
	if len(mapLoaded) != 3 || len(sliceLoaded) != 3 {
		t.Fatalf("loaded ids map=%d slice=%d, want 3 each", len(mapLoaded), len(sliceLoaded))
	}
	for id := range mapLoaded {
		if !sliceLoaded[id] {
			t.Errorf("slice store missing id %q", id)
		}
	}
}
