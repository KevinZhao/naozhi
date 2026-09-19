package discovery

import (
	"sync"
	"testing"
)

// TestGetCachedSummary_LockFreeGenRefresh verifies the R202606h-PERF-011
// (#2330) change: a cache hit refreshes the entry's generation without taking
// the write lock and survives eviction across many scan generations. The hit
// path must keep the entry's gen current so evictSummaryCache does not drop a
// still-hot entry.
func TestGetCachedSummary_LockFreeGenRefresh(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const path = "/p/index.json"
	const mtime int64 = 42

	sc.setCachedSummary(path, mtime, sessionsIndex{OriginalPath: "/tmp/p"})

	for i := 0; i < 5; i++ {
		sc.summaryCache.generation.Add(1)
		idx, ok := sc.getCachedSummary(path, mtime)
		if !ok || idx.OriginalPath != "/tmp/p" {
			t.Fatalf("iter %d: getCachedSummary ok=%v path=%q", i, ok, idx.OriginalPath)
		}
	}

	sc.summaryCache.RLock()
	e := sc.summaryCache.entries[path]
	cur := sc.summaryCache.generation.Load()
	sc.summaryCache.RUnlock()
	if e.gen.Load() != cur {
		t.Errorf("entry gen = %d, want current generation %d", e.gen.Load(), cur)
	}
}

// TestGetCachedSummary_ConcurrentHits exercises many goroutines refreshing the
// same entry concurrently while the generation advances. With -race this
// catches any unsynchronised access — the gen refresh must be a lock-free
// atomic Store, never a racy map write.
func TestGetCachedSummary_ConcurrentHits(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const path = "/p/shared-index.json"
	const mtime int64 = 7
	sc.setCachedSummary(path, mtime, sessionsIndex{OriginalPath: "/tmp/shared"})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if idx, ok := sc.getCachedSummary(path, mtime); !ok || idx.OriginalPath != "/tmp/shared" {
					t.Errorf("getCachedSummary ok=%v path=%q", ok, idx.OriginalPath)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			sc.summaryCache.generation.Add(1)
		}
	}()
	wg.Wait()
}

// TestSetCachedSummary_StableEntryAddress verifies the map stores
// *summaryCacheEntry so the entry (and &gen) address is stable across hit
// refreshes — the precondition for the lock-free Store (#2330).
func TestSetCachedSummary_StableEntryAddress(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	const path = "/p/stable-index.json"
	const mtime int64 = 11
	sc.setCachedSummary(path, mtime, sessionsIndex{OriginalPath: "/tmp/s"})

	sc.summaryCache.RLock()
	e1 := sc.summaryCache.entries[path]
	sc.summaryCache.RUnlock()
	genAddr := &e1.gen

	for i := 0; i < 10; i++ {
		sc.summaryCache.generation.Add(1)
		if _, ok := sc.getCachedSummary(path, mtime); !ok {
			t.Fatalf("iter %d: expected cache hit", i)
		}
		sc.summaryCache.RLock()
		e2 := sc.summaryCache.entries[path]
		sc.summaryCache.RUnlock()
		if e2 != e1 {
			t.Fatalf("iter %d: entry pointer changed on hit refresh", i)
		}
		if &e2.gen != genAddr {
			t.Fatalf("iter %d: &gen address moved on hit refresh", i)
		}
	}
}
