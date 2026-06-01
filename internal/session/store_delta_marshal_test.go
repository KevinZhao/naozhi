package session

import (
	"encoding/json"
	"testing"
)

// collectEntries mirrors the legacy saveStore body: build the []storeEntry in
// map-range order and json.Marshal it. Used to assert marshalStoreEntries is
// byte-identical to the pre-PERF-2 path for the same iteration order.
func collectEntries(sessions map[string]*ManagedSession) []storeEntry {
	entries := make([]storeEntry, 0, len(sessions))
	for _, s := range sessions {
		entry, ok := sessionToStoreEntry(s)
		if !ok {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// TestMarshalStoreEntries_ByteEquivalent pins R20260531A-PERF-2 (#1523): the
// incremental array assembly must produce exactly the same JSON the old
// json.Marshal([]storeEntry) produced. We compare by re-parsing both into a
// map keyed by Key so map-iteration order does not make the test flaky.
func TestMarshalStoreEntries_ByteEquivalent(t *testing.T) {
	a := newSessionWithID("feishu:direct:alice:general", "sess-1")
	a.SetUserLabel("alpha")
	b := newSessionWithID("feishu:group:room:general", "sess-2")
	b.historyMu.Lock()
	b.prevSessionIDs = []string{"old-1", "old-2"}
	b.prevSessionOrigins = []string{"manual", "auto-spawn"}
	b.historyMu.Unlock()

	sessions := map[string]*ManagedSession{a.key: a, b.key: b}

	got, err := marshalStoreEntries(sessions)
	if err != nil {
		t.Fatalf("marshalStoreEntries error: %v", err)
	}
	want, err := json.Marshal(collectEntries(sessions))
	if err != nil {
		t.Fatalf("reference marshal error: %v", err)
	}

	var gotEntries, wantEntries []storeEntry
	if err := json.Unmarshal(got, &gotEntries); err != nil {
		t.Fatalf("unmarshal got: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &wantEntries); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	toMap := func(es []storeEntry) map[string]storeEntry {
		m := make(map[string]storeEntry, len(es))
		for _, e := range es {
			m[e.Key] = e
		}
		return m
	}
	gm, wm := toMap(gotEntries), toMap(wantEntries)
	if len(gm) != len(wm) {
		t.Fatalf("entry count: got %d want %d", len(gm), len(wm))
	}
	for k, we := range wm {
		ge, ok := gm[k]
		if !ok {
			t.Fatalf("missing key %q in marshalStoreEntries output", k)
		}
		if !equalStoreEntry(ge, we) {
			t.Errorf("entry %q differs:\n got=%+v\nwant=%+v", k, ge, we)
		}
	}
}

// TestEncodeStoreEntryCached_ReusesCache verifies the memo: a second call with
// no field change returns the SAME backing slice (cache hit), while a mutation
// invalidates it and produces fresh bytes.
func TestEncodeStoreEntryCached_ReusesCache(t *testing.T) {
	s := newSessionWithID("feishu:direct:alice:general", "sess-1")
	s.lastActive.Store(1000)

	d1, ok := encodeStoreEntryCached(s)
	if !ok {
		t.Fatal("first encode should succeed")
	}
	d2, ok := encodeStoreEntryCached(s)
	if !ok {
		t.Fatal("second encode should succeed")
	}
	// Cache hit: identical backing array (same pointer + len).
	if &d1[0] != &d2[0] {
		t.Error("unchanged session should hit the marshal cache (same backing slice)")
	}

	// Mutate a persisted field; cache must invalidate and re-marshal.
	s.lastActive.Store(2000)
	d3, ok := encodeStoreEntryCached(s)
	if !ok {
		t.Fatal("third encode should succeed")
	}
	if len(d3) > 0 && len(d1) > 0 && &d3[0] == &d1[0] {
		t.Error("changed session must NOT reuse the stale cached slice")
	}
	var e storeEntry
	if err := json.Unmarshal(d3, &e); err != nil {
		t.Fatalf("re-marshalled entry not valid JSON: %v", err)
	}
	if e.LastActive != 2000 {
		t.Errorf("re-marshalled LastActive = %d, want 2000", e.LastActive)
	}
}

// TestMarshalStoreEntries_Empty verifies an all-skipped / empty map yields the
// canonical empty array, matching json.Marshal([]storeEntry{}).
func TestMarshalStoreEntries_Empty(t *testing.T) {
	got, err := marshalStoreEntries(map[string]*ManagedSession{})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("empty marshal = %q, want %q", got, "[]")
	}

	// A map whose only entry is skipped (no session ID) must also be "[]".
	skipOnly := map[string]*ManagedSession{"k": {key: "k"}}
	got, err = marshalStoreEntries(skipOnly)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("skip-only marshal = %q, want %q", got, "[]")
	}
}
