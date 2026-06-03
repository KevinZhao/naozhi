package session

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestMarshalStoreEntriesSlice_MatchesMap pins R20260602190132-PERF-4 (#1606):
// the slice-input marshal path used by the periodic save must produce the same
// logical set of entries as the map-input path for the same sessions.
func TestMarshalStoreEntriesSlice_MatchesMap(t *testing.T) {
	a := newSessionWithID("feishu:direct:alice:general", "sess-1")
	a.SetUserLabel("alpha")
	b := newSessionWithID("feishu:group:room:general", "sess-2")

	asMap := map[string]*ManagedSession{a.key: a, b.key: b}
	asSlice := []*ManagedSession{a, b}

	mapBytes, err := marshalStoreEntries(asMap)
	if err != nil {
		t.Fatalf("marshalStoreEntries error: %v", err)
	}
	sliceBytes, err := marshalStoreEntriesSlice(asSlice)
	if err != nil {
		t.Fatalf("marshalStoreEntriesSlice error: %v", err)
	}

	toMap := func(raw []byte) map[string]storeEntry {
		var es []storeEntry
		if err := json.Unmarshal(raw, &es); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		m := make(map[string]storeEntry, len(es))
		for _, e := range es {
			m[e.Key] = e
		}
		return m
	}
	gm, wm := toMap(sliceBytes), toMap(mapBytes)
	if len(gm) != len(wm) {
		t.Fatalf("entry count: slice %d want %d", len(gm), len(wm))
	}
	for k, we := range wm {
		ge, ok := gm[k]
		if !ok {
			t.Fatalf("slice output missing key %q", k)
		}
		if !equalStoreEntry(ge, we) {
			t.Errorf("entry %q differs:\n slice=%+v\n  map=%+v", k, ge, we)
		}
	}
}

// TestMarshalStoreEntriesSlice_Empty mirrors the map-path empty/skip behaviour.
func TestMarshalStoreEntriesSlice_Empty(t *testing.T) {
	got, err := marshalStoreEntriesSlice(nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("empty slice marshal = %q, want %q", got, "[]")
	}
	// A session with no session ID is skipped → still "[]".
	got, err = marshalStoreEntriesSlice([]*ManagedSession{{key: "k"}})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("skip-only slice marshal = %q, want %q", got, "[]")
	}
}

// TestSaveStoreSlice_RoundTrip verifies saveStoreSlice writes a store that
// loadStore can read back, i.e. the slice save path is on-disk compatible with
// the map save path.
func TestSaveStoreSlice_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	a := newSessionWithID("feishu:direct:alice:general", "sess-1")
	b := newSessionWithID("feishu:group:room:general", "sess-2")

	if err := saveStoreSlice(path, []*ManagedSession{a, b}); err != nil {
		t.Fatalf("saveStoreSlice error: %v", err)
	}

	loaded := loadStore(path)
	if len(loaded) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(loaded))
	}
	if _, ok := loaded[a.key]; !ok {
		t.Errorf("loaded store missing key %q", a.key)
	}
	if _, ok := loaded[b.key]; !ok {
		t.Errorf("loaded store missing key %q", b.key)
	}
}

// TestSaveStoreSlice_EmptyPath is a no-op (matches saveStore("", ...)).
func TestSaveStoreSlice_EmptyPath(t *testing.T) {
	if err := saveStoreSlice("", []*ManagedSession{{key: "k"}}); err != nil {
		t.Errorf("saveStoreSlice(\"\") error: %v", err)
	}
}
