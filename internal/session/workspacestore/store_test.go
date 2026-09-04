package workspacestore

import (
	"fmt"
	"testing"
)

func TestZeroValue_ReadsAreSafe(t *testing.T) {
	var s Store
	if _, ok := s.Lookup("x"); ok {
		t.Error("Lookup on zero Store must miss")
	}
	if s.Len() != 0 || s.Dirty() || s.Gen() != 0 {
		t.Errorf("zero Store: Len=%d Dirty=%v Gen=%d", s.Len(), s.Dirty(), s.Gen())
	}
	if snap := s.Snapshot(); snap == nil || len(snap) != 0 {
		t.Errorf("Snapshot on zero Store must be empty and non-nil, got %#v", snap)
	}
	calls := 0
	s.Range(func(string, string) { calls++ })
	if calls != 0 {
		t.Errorf("Range on zero Store called fn %d times", calls)
	}
	if s.Delete("x") {
		t.Error("Delete on zero Store must report false")
	}
	if err := s.CheckInvariants(); err != nil {
		t.Error(err)
	}
}

func TestSet_StampsSeqAndDirties(t *testing.T) {
	var s Store
	s.Set("a", "/a")
	if ws, ok := s.Lookup("a"); !ok || ws != "/a" {
		t.Fatalf("Lookup(a)=%q,%v", ws, ok)
	}
	if !s.Dirty() || s.Gen() != 1 {
		t.Errorf("after Set: Dirty=%v Gen=%d want true,1", s.Dirty(), s.Gen())
	}
	if s.seq["a"] != 1 {
		t.Errorf("seq[a]=%d want 1", s.seq["a"])
	}
	// Re-setting an existing key updates in place and re-stamps as newest.
	s.Set("b", "/b")
	s.Set("a", "/a2")
	if s.Len() != 2 || s.seq["a"] != 3 || s.Gen() != 3 {
		t.Errorf("Len=%d seq[a]=%d Gen=%d want 2,3,3", s.Len(), s.seq["a"], s.Gen())
	}
}

func TestSeed_NoDirtyNoSeq(t *testing.T) {
	var s Store
	s.Seed(map[string]string{"a": "/a", "b": "/b"})
	if s.Len() != 2 {
		t.Fatalf("Len=%d want 2", s.Len())
	}
	if s.Dirty() || s.Gen() != 0 {
		t.Errorf("Seed must not dirty/bump: Dirty=%v Gen=%d", s.Dirty(), s.Gen())
	}
	if len(s.seq) != 0 {
		t.Errorf("Seed must not stamp seq, got %v", s.seq)
	}
	s.Seed(nil) // no-op
	if s.Len() != 2 {
		t.Errorf("Seed(nil) changed Len to %d", s.Len())
	}
}

func TestAdopt_DirtiesOnlyOnChange(t *testing.T) {
	var s Store
	if !s.Adopt("a", "/a") {
		t.Fatal("first Adopt must report changed")
	}
	if !s.Dirty() || s.Gen() != 1 {
		t.Fatalf("Dirty=%v Gen=%d", s.Dirty(), s.Gen())
	}
	s.MarkSavedIfUnchanged(s.Gen())
	if s.Adopt("a", "/a") {
		t.Error("same value must report unchanged")
	}
	if s.Dirty() || s.Gen() != 1 {
		t.Errorf("unchanged Adopt must not dirty/bump: Dirty=%v Gen=%d", s.Dirty(), s.Gen())
	}
	if !s.Adopt("a", "/changed") || !s.Dirty() || s.Gen() != 2 {
		t.Errorf("changed Adopt: Dirty=%v Gen=%d", s.Dirty(), s.Gen())
	}
	if _, stamped := s.seq["a"]; stamped {
		t.Error("Adopt must not stamp seq (sorts oldest for eviction)")
	}
}

func TestDelete_RemovesSeqAndDirties(t *testing.T) {
	var s Store
	s.Set("a", "/a")
	s.MarkSavedIfUnchanged(s.Gen())
	if s.Delete("missing") {
		t.Error("Delete(missing) must report false")
	}
	if s.Dirty() {
		t.Error("Delete(missing) must not dirty")
	}
	if !s.Delete("a") {
		t.Fatal("Delete(a) must report true")
	}
	if s.Len() != 0 || len(s.seq) != 0 || !s.Dirty() || s.Gen() != 2 {
		t.Errorf("after Delete: Len=%d seq=%d Dirty=%v Gen=%d", s.Len(), len(s.seq), s.Dirty(), s.Gen())
	}
	if err := s.CheckInvariants(); err != nil {
		t.Error(err)
	}
}

func TestSnapshot_IsIndependentCopy(t *testing.T) {
	var s Store
	s.Set("a", "/a")
	snap := s.Snapshot()
	snap["a"] = "/mutated"
	snap["b"] = "/new"
	if ws, _ := s.Lookup("a"); ws != "/a" || s.Len() != 1 {
		t.Errorf("Snapshot aliased the live map: Lookup(a)=%q Len=%d", ws, s.Len())
	}
}

func TestMarkSavedIfUnchanged(t *testing.T) {
	var s Store
	s.Set("a", "/a")
	snapGen := s.Gen()
	s.Set("b", "/b") // concurrent mutation between snapshot and save
	if s.MarkSavedIfUnchanged(snapGen) {
		t.Error("must not clear dirty when gen advanced")
	}
	if !s.Dirty() {
		t.Error("dirty must stay set")
	}
	if !s.MarkSavedIfUnchanged(s.Gen()) || s.Dirty() {
		t.Error("must clear dirty when gen unchanged")
	}
}

func TestSetBounded_ExistingKeyUpdatesWithoutEviction(t *testing.T) {
	var s Store
	const capacity = 3
	for i := 0; i < capacity; i++ {
		s.SetBounded(fmt.Sprintf("k%d", i), "/ws", capacity, nil)
	}
	if !s.SetBounded("k0", "/ws-new", capacity, nil) {
		t.Fatal("update of existing key at capacity must succeed")
	}
	if s.Len() != capacity {
		t.Errorf("Len=%d want %d", s.Len(), capacity)
	}
	for i := 0; i < capacity; i++ {
		if _, ok := s.Lookup(fmt.Sprintf("k%d", i)); !ok {
			t.Errorf("k%d evicted on in-place update", i)
		}
	}
}

func TestSetBounded_EvictsLeastRecentlySetSessionless(t *testing.T) {
	var s Store
	const capacity = 4
	for i := 0; i < capacity; i++ {
		s.SetBounded(fmt.Sprintf("k%d", i), "/ws", capacity, nil)
	}
	// k0 is oldest but live → k1 is the victim.
	live := func(k string) bool { return k == "k0" }
	if !s.SetBounded("fresh", "/fresh", capacity, live) {
		t.Fatal("new key at capacity must be accepted via eviction")
	}
	if _, ok := s.Lookup("k0"); !ok {
		t.Error("live k0 must never be evicted")
	}
	if _, ok := s.Lookup("k1"); ok {
		t.Error("oldest session-less k1 must be the victim")
	}
	if _, ok := s.Lookup("fresh"); !ok {
		t.Error("fresh override dropped")
	}
	if s.Len() != capacity {
		t.Errorf("Len=%d want %d (cap held)", s.Len(), capacity)
	}
	if err := s.CheckInvariants(); err != nil {
		t.Error(err)
	}
}

func TestSetBounded_UnstampedKeysEvictedFirst(t *testing.T) {
	var s Store
	const capacity = 3
	s.Seed(map[string]string{"disk0": "/d0", "disk1": "/d1"})
	s.SetBounded("seqd", "/seqd", capacity, nil)
	if !s.SetBounded("fresh", "/fresh", capacity, nil) {
		t.Fatal("expected eviction to make room")
	}
	if _, ok := s.Lookup("seqd"); !ok {
		t.Error("seq-stamped key evicted before disk-loaded keys")
	}
	if _, ok := s.Lookup("fresh"); !ok {
		t.Error("fresh key missing")
	}
	disk := 0
	for _, k := range []string{"disk0", "disk1"} {
		if _, ok := s.Lookup(k); ok {
			disk++
		}
	}
	if disk != 1 {
		t.Errorf("exactly one disk-loaded key must survive, got %d", disk)
	}
}

func TestSetBounded_DropsWhenAllLive(t *testing.T) {
	var s Store
	const capacity = 2
	s.Set("a", "/a")
	s.Set("b", "/b")
	gen := s.Gen()
	allLive := func(string) bool { return true }
	if s.SetBounded("c", "/c", capacity, allLive) {
		t.Fatal("must drop when every override is live")
	}
	if s.Len() != capacity {
		t.Errorf("Len=%d want %d — must never exceed cap", s.Len(), capacity)
	}
	if _, ok := s.Lookup("c"); ok {
		t.Error("dropped key must not be present")
	}
	if s.Gen() != gen {
		t.Error("a dropped write must not bump gen")
	}
}

func TestRange_VisitsAll(t *testing.T) {
	var s Store
	s.Seed(map[string]string{"a": "/a", "b": "/b"})
	seen := map[string]string{}
	s.Range(func(k, v string) { seen[k] = v })
	if len(seen) != 2 || seen["a"] != "/a" || seen["b"] != "/b" {
		t.Errorf("Range saw %v", seen)
	}
}

func TestCheckInvariants_DetectsSeqOrphan(t *testing.T) {
	var s Store
	s.Set("a", "/a")
	delete(s.overrides, "a") // corrupt: seq outlives override
	if err := s.CheckInvariants(); err == nil {
		t.Error("expected invariant violation for orphaned seq stamp")
	}
}
