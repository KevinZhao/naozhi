package feishu

import (
	"strconv"
	"sync"
	"testing"
)

// liveNonceCount counts the actual entries in seenNonces (test helper).
func liveNonceCount(f *Feishu) int64 {
	var n int64
	f.seenNonces.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestEvictOldestNonces_ConcurrentEvictNoNegativeCount pins R20260531070014-SEC-4
// (#1534): when many goroutines hit the cap and call evictOldestNonces at once,
// the serialized evict+recount must keep seenNoncesCount non-negative and, once
// all evictions drain, exactly equal to the map's real size. The pre-fix code
// did a relative Add(-deleted) per goroutine, which under overlapping Range
// snapshots could drive the counter below the true map size or negative.
func TestEvictOldestNonces_ConcurrentEvictNoNegativeCount(t *testing.T) {
	t.Parallel()
	f := &Feishu{}

	// Seed well above the eviction batch so every concurrent evict actually
	// removes a full batch and their Range snapshots overlap heavily.
	total := nonceEvictionBatch * 8
	for i := 0; i < total; i++ {
		f.seenNonces.Store("k-"+strconv.Itoa(i), int64(i))
	}
	f.seenNoncesCount.Store(int64(total))

	const goroutines = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	minSeen := int64(total)

	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			f.evictOldestNonces()
			// Sample the counter right after this goroutine's evict; it must
			// never be observed negative.
			c := f.seenNoncesCount.Load()
			mu.Lock()
			if c < minSeen {
				minSeen = c
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if minSeen < 0 {
		t.Fatalf("seenNoncesCount observed negative during concurrent eviction: %d", minSeen)
	}
	// After all evictions settle, the counter must match the real map size.
	got := f.seenNoncesCount.Load()
	live := liveNonceCount(f)
	if got != live {
		t.Fatalf("after concurrent eviction seenNoncesCount=%d but live map size=%d", got, live)
	}
	if got < 0 {
		t.Fatalf("final seenNoncesCount is negative: %d", got)
	}
}

// TestEvictOldestNonces_RecountResyncsToMapSize verifies the post-fix invariant
// in isolation: even if the counter is drifted ABOVE the true size (e.g. leaked
// speculative reservations from racing inserts), a single evict resyncs it to
// the map's actual live size rather than blindly subtracting the delete count.
func TestEvictOldestNonces_RecountResyncsToMapSize(t *testing.T) {
	t.Parallel()
	f := &Feishu{}

	total := nonceEvictionBatch + 500
	for i := 0; i < total; i++ {
		f.seenNonces.Store("k-"+strconv.Itoa(i), int64(i))
	}
	// Drift the counter high — simulating overlapping speculative +1s that a
	// relative Add(-deleted) would fail to reconcile.
	f.seenNoncesCount.Store(int64(total + 9999))

	deleted := f.evictOldestNonces()
	if deleted != nonceEvictionBatch {
		t.Fatalf("deleted=%d; want %d", deleted, nonceEvictionBatch)
	}
	got := f.seenNoncesCount.Load()
	live := liveNonceCount(f)
	if got != live {
		t.Fatalf("counter=%d not resynced to live map size=%d", got, live)
	}
	if got != int64(total-nonceEvictionBatch) {
		t.Fatalf("counter=%d; want %d", got, total-nonceEvictionBatch)
	}
}
