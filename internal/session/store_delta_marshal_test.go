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

// TestEncodeStoreEntryCached_ChainMutationInvalidates is the regression guard
// for R202606j-PERF-014 (#2346): equalStoreEntry now compares the
// prevHistoryGen counter instead of slices.Equal over PrevSessionIDs /
// PrevSessionOrigins. A chain mutation MUST bump the gen so the marshal cache
// invalidates and the new chain is persisted — otherwise a /clear or auto-chain
// rotation would silently keep serving the stale cached encoding.
func TestEncodeStoreEntryCached_ChainMutationInvalidates(t *testing.T) {
	s := newSessionWithID("feishu:direct:bob:general", "sess-1")
	s.lastActive.Store(1000)

	d1, ok := encodeStoreEntryCached(s)
	if !ok {
		t.Fatal("first encode should succeed")
	}

	// Mutate the prev-session chain via the dedicated mutator (bumps gen).
	s.ReplacePrevSessionIDs([]string{"old-1", "old-2"})

	d2, ok := encodeStoreEntryCached(s)
	if !ok {
		t.Fatal("second encode should succeed")
	}
	if len(d2) > 0 && len(d1) > 0 && &d2[0] == &d1[0] {
		t.Fatal("chain mutation must invalidate the marshal cache (stale slice reused)")
	}
	var e storeEntry
	if err := json.Unmarshal(d2, &e); err != nil {
		t.Fatalf("re-marshalled entry not valid JSON: %v", err)
	}
	if len(e.PrevSessionIDs) != 2 || e.PrevSessionIDs[0] != "old-1" {
		t.Errorf("re-marshalled PrevSessionIDs = %v, want [old-1 old-2]", e.PrevSessionIDs)
	}
}

// TestEqualStoreEntry_GenDistinguishesChains verifies the gen-based comparison:
// two entries built from the same session before and after a chain mutation must
// compare unequal (different prevGen), and two encodes with no chain change must
// compare equal.
func TestEqualStoreEntry_GenDistinguishesChains(t *testing.T) {
	s := newSessionWithID("feishu:direct:carol:general", "sess-1")
	s.ReplacePrevSessionIDs([]string{"p1", "p2"})

	e1, ok := sessionToStoreEntry(s)
	if !ok {
		t.Fatal("first conversion should succeed")
	}
	e1b, _ := sessionToStoreEntry(s)
	if !equalStoreEntry(e1, e1b) {
		t.Error("two conversions with no mutation must be equal")
	}

	// Stamp the trailing chain entry's origin — a real chain mutation that
	// bumps prevHistoryGen (start = len(prevSessionIDs)-len(ids) = 1 >= 0).
	s.SetPrevSessionOrigins([]string{"p2"}, "auto-spawn")
	e2, _ := sessionToStoreEntry(s)
	if equalStoreEntry(e1, e2) {
		t.Error("post-mutation entry must differ from pre-mutation entry (gen bumped)")
	}
}
