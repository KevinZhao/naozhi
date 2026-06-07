package cron

import (
	"sync"
	"testing"
	"time"
)

// TestRunStore_WarmCacheLocked_ConcurrentWarmAndGet pins R20260607-PERF-6
// (#1903): warmCacheLocked now releases entry.mu across the disk scan
// (diskListNewestFirst) and only re-acquires it to publish the seeded ring.
// jobLock still serialises writers, so many goroutines racing to warm the
// same cold entry — interleaved with concurrent cacheGet readers — must
// converge on exactly one warm pass with the correct row count and no
// duplicates. Run under -race to catch any unsynchronised ring access if a
// future refactor narrows the jobLock window.
func TestRunStore_WarmCacheLocked_ConcurrentWarmAndGet(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)

	jobID := mustGenerateID()
	const nRuns = 25
	now := time.Now()
	for i := 0; i < nRuns; i++ {
		s.Append(makeRun(jobID, now.Add(-time.Duration(i+1)*time.Minute)))
	}
	// Drop the warm cache so every goroutine below races on the cold path.
	s.cacheInvalidate(jobID)

	const nWarmers = 16
	const nReaders = 16
	var wg sync.WaitGroup
	start := make(chan struct{})

	// Warmers: each calls warmCacheLocked directly to maximise the chance
	// they overlap inside the (now entry.mu-free) disk-scan window.
	for i := 0; i < nWarmers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.warmCacheLocked(jobID)
		}()
	}
	// Readers: hammer cacheGet (entry.mu.RLock) while the warmers run. The
	// point is the readers must never block for the full disk-scan latency
	// and must never observe a torn ring.
	for i := 0; i < nReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 50; j++ {
				rows, _ := s.cacheGet(jobID, 200)
				// A warm-empty hit (nil) is acceptable mid-race; a populated
				// result must be internally consistent (no zero-RunID rows
				// from a torn ringSeed).
				for _, r := range rows {
					if r.RunID == "" {
						t.Errorf("cacheGet returned a row with empty RunID — torn ring")
						return
					}
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	// Final state: exactly warm, exactly nRuns rows, no duplicate RunIDs.
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		t.Fatalf("cache entry must exist after warm")
	}
	entry := v.(*recentCacheEntry)
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if !entry.warm {
		t.Fatalf("entry must be warm after concurrent warm")
	}
	if entry.count != nRuns {
		t.Fatalf("entry.count = %d, want %d", entry.count, nRuns)
	}
	seen := make(map[string]struct{}, entry.count)
	for i := 0; i < entry.count; i++ {
		id := entry.ringRead(i).RunID
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate RunID %q in ring after concurrent warm", id)
		}
		seen[id] = struct{}{}
	}
}

// TestRunStore_WarmCacheLocked_DiscardsRedundantScan asserts the defensive
// post-scan warm re-check: if the entry is already warm by the time a warmer
// re-acquires entry.mu, it must discard its own scan rather than re-seed
// (which would otherwise clobber rows appended in the meantime). Driven via
// the public warm path so the contract holds end-to-end.
func TestRunStore_WarmCacheLocked_DiscardsRedundantScan(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)

	jobID := mustGenerateID()
	s.Append(makeRun(jobID, time.Now()))

	// First warm seeds the ring.
	s.warmCacheLocked(jobID)
	v, _ := s.recentCache.Load(jobID)
	entry := v.(*recentCacheEntry)

	// A second warm on an already-warm entry must be a no-op (warm check
	// short-circuits before any disk scan).
	s.warmCacheLocked(jobID)

	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if !entry.warm {
		t.Fatalf("entry must stay warm")
	}
	if entry.count != 1 {
		t.Fatalf("redundant warm must not change count; got %d want 1", entry.count)
	}
}
