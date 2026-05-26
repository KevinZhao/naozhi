package server

import (
	"sync"
	"testing"
)

// TestHistoryMarshalCache_SlotIsStablePerKey pins R250-PERF-28 (#1131):
// the sync.Map migration must keep slot(key) returning the *same*
// *marshalCacheEntry pointer for repeated calls on a live key, so the
// per-key fingerprint update inside getOrMarshal observes earlier
// writers' state. A regression that recreated the entry on every call
// would lose the fingerprint and degrade the cache to "always miss"
// without altering any visible API shape.
func TestHistoryMarshalCache_SlotIsStablePerKey(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	a := cache.slot("feishu:p2p:userX")
	b := cache.slot("feishu:p2p:userX")
	if a != b {
		t.Fatalf("slot returned different *marshalCacheEntry on repeat call (a=%p b=%p) — fingerprint memo would be lost", a, b)
	}

	// drop must release the slot so a follow-up call gets a fresh entry.
	cache.drop("feishu:p2p:userX")
	c := cache.slot("feishu:p2p:userX")
	if c == a {
		t.Fatal("slot returned the dropped entry — drop did not release the sync.Map key")
	}
}

// TestHistoryMarshalCache_ConcurrentSlotRace exercises the LoadOrStore
// race (two goroutines slot()ing the same fresh key concurrently) with
// -race; both callers must end up with the same *marshalCacheEntry so
// only one fingerprint memo is live per key. R250-PERF-28 (#1131).
func TestHistoryMarshalCache_ConcurrentSlotRace(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	const N = 32
	var wg sync.WaitGroup
	results := make([]*marshalCacheEntry, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx] = cache.slot("feishu:p2p:hot")
		}(i)
	}
	wg.Wait()

	first := results[0]
	if first == nil {
		t.Fatal("slot returned nil")
	}
	for i, r := range results {
		if r != first {
			t.Errorf("goroutine %d slot returned %p, want %p (LoadOrStore must serialise)", i, r, first)
		}
	}
}

// TestHistoryMarshalCache_ResetClearsAllKeys pins that reset() releases
// every cached slot so Hub.Shutdown stops pinning the largest payloads.
// R250-PERF-28 (#1131): the sync.Map.Range loop must visit and Delete
// each key — not just zero out a map field — so the post-reset slot
// call returns a fresh *marshalCacheEntry pointer.
func TestHistoryMarshalCache_ResetClearsAllKeys(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	a1 := cache.slot("k1")
	a2 := cache.slot("k2")
	if a1 == a2 {
		t.Fatal("distinct keys must yield distinct entries")
	}

	cache.reset()

	// After reset both keys must regenerate; the new entries cannot equal
	// the originals because reset removed them from the sync.Map.
	b1 := cache.slot("k1")
	b2 := cache.slot("k2")
	if b1 == a1 {
		t.Errorf("reset did not release k1 entry: still %p", b1)
	}
	if b2 == a2 {
		t.Errorf("reset did not release k2 entry: still %p", b2)
	}
}
