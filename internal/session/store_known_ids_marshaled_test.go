package session

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestSnapshotKnownIDsMarshaled_MemoisedAndRefreshed pins R20260616-PERF-009
// (#2143): snapshotKnownIDsMarshaledLocked must (a) return valid JSON matching
// the sorted set, (b) reuse its cached marshaled bytes when knownIDsGen is
// unchanged (skipping json.Marshal on the throttled save tick), and (c)
// re-marshal when a new ID bumps the gen. We observe the cache reuse by
// checking the cached backing byte slice identity does not change across
// repeated snapshots of an unchanged set.
func TestSnapshotKnownIDsMarshaled_MemoisedAndRefreshed(t *testing.T) {
	r := &Router{kid: knownIDsStore{ids: make(map[string]bool)}}
	for _, id := range []string{"ccc", "aaa", "bbb"} {
		r.trackSessionID(id)
	}

	got1, err := r.snapshotKnownIDsMarshaledLocked()
	if err != nil {
		t.Fatalf("first marshal snapshot: %v", err)
	}
	var decoded []string
	if err := json.Unmarshal(got1, &decoded); err != nil {
		t.Fatalf("returned bytes are not valid JSON: %v (%q)", err, got1)
	}
	want := []string{"aaa", "bbb", "ccc"}
	if !slices.Equal(decoded, want) {
		t.Fatalf("decoded = %v, want sorted %v", decoded, want)
	}

	// Cache must be built and tagged with the current gen.
	if r.kid.marshaledCache == nil {
		t.Fatal("marshaledCache not populated after first snapshot")
	}
	if r.kid.marshaledGen != r.kid.gen {
		t.Fatalf("marshaledGen %d != gen %d after first snapshot", r.kid.marshaledGen, r.kid.gen)
	}
	cachePtr1 := r.kid.marshaledCache

	// Second snapshot with NO mutation: the cached bytes must be reused (no
	// re-marshal), though the returned slice is a fresh clone.
	got2, err := r.snapshotKnownIDsMarshaledLocked()
	if err != nil {
		t.Fatalf("second marshal snapshot: %v", err)
	}
	if string(got2) != string(got1) {
		t.Errorf("second snapshot bytes differ: %q vs %q", got2, got1)
	}
	if &r.kid.marshaledCache[0] != &cachePtr1[0] {
		t.Error("marshaled cache backing array was rebuilt despite no mutation (memoisation broken)")
	}
	// Returned slice must be a distinct copy, not the cache itself, so a
	// concurrent rebuild cannot corrupt an in-flight save.
	if len(got2) > 0 && &got2[0] == &r.kid.marshaledCache[0] {
		t.Error("snapshot aliases the marshaled cache; concurrent rebuild could corrupt the in-flight save")
	}

	// Track a new ID → gen bumps → next snapshot re-marshals.
	r.trackSessionID("aab")
	got3, err := r.snapshotKnownIDsMarshaledLocked()
	if err != nil {
		t.Fatalf("post-mutation marshal snapshot: %v", err)
	}
	var decoded3 []string
	if err := json.Unmarshal(got3, &decoded3); err != nil {
		t.Fatalf("post-mutation bytes not valid JSON: %v", err)
	}
	want3 := []string{"aaa", "aab", "bbb", "ccc"}
	if !slices.Equal(decoded3, want3) {
		t.Errorf("post-mutation decoded = %v, want %v", decoded3, want3)
	}
	if r.kid.marshaledGen != r.kid.gen {
		t.Errorf("marshaledGen not advanced after mutation: %d vs %d", r.kid.marshaledGen, r.kid.gen)
	}
	if string(got3) == string(got1) {
		t.Error("bytes unchanged after a new ID was tracked (stale marshal returned)")
	}
}

// TestSnapshotKnownIDsMarshaled_MatchesSaveKnownIDs verifies the memoised
// marshal produces byte-identical output to the legacy saveKnownIDs marshal
// path, so swapping the save call to saveKnownIDsBytes preserves the on-disk
// R180-GO-P2 stable-bytes contract.
func TestSnapshotKnownIDsMarshaled_MatchesSaveKnownIDs(t *testing.T) {
	r := &Router{kid: knownIDsStore{ids: make(map[string]bool)}}
	for _, id := range []string{"zeta", "alpha", "mike", "delta"} {
		r.trackSessionID(id)
	}

	memoised, err := r.snapshotKnownIDsMarshaledLocked()
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	// Independently marshal the sorted slice the way saveKnownIDs did.
	legacy, err := json.Marshal(r.snapshotKnownIDsSortedLocked())
	if err != nil {
		t.Fatalf("legacy marshal: %v", err)
	}
	if string(memoised) != string(legacy) {
		t.Errorf("memoised marshal differs from legacy:\n memoised %q\n legacy   %q", memoised, legacy)
	}
}
