package cron

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRunStore_AppendConcurrentSameJob_NoRingDup pins R20260527122801-PERF-4
// (#1335): with WriteFileAtomic now hoisted out of jobLock, a concurrent
// warmCache CAN read a freshly-renamed run file from disk before the
// matching cacheHeadPush runs. The dedup-by-RunID inside cacheHeadPush
// guarantees we don't end up with duplicate ring entries. This test
// drives N concurrent Appends + a forced warmCache and asserts the cache
// holds N distinct RunIDs (no duplicates) and N rows.
func TestRunStore_AppendConcurrentSameJob_NoRingDup(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	store := newRunStore(storePath, 100, 24*time.Hour)
	if store.disabled {
		t.Fatal("store disabled")
	}

	jobID := "deadbeefcafe1234"

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runID := fmt.Sprintf("%016x", uint64(0x100000000)+uint64(idx))
			store.Append(&CronRun{
				JobID:     jobID,
				RunID:     runID,
				State:     RunStateSucceeded,
				StartedAt: time.Now(),
				EndedAt:   time.Now(),
			})
		}(i)
		// Half-way through, race a warmCache on top of the in-flight
		// Appends so the cacheHeadPush interleave path actually fires.
		if i == n/2 {
			go store.warmCache(jobID)
		}
	}
	wg.Wait()
	// Final warmCache flush in case the inline one already won the race.
	store.warmCache(jobID)

	got := store.Recent(jobID, 200)
	if len(got) != n {
		t.Errorf("Recent count: got %d want %d", len(got), n)
	}
	seen := make(map[string]int, len(got))
	for _, r := range got {
		seen[r.RunID]++
	}
	for id, c := range seen {
		if c > 1 {
			t.Errorf("duplicate RunID %q in cache (count=%d)", id, c)
		}
	}
	if len(seen) != n {
		t.Errorf("distinct RunIDs in cache: got %d want %d", len(seen), n)
	}
}

// TestRunStore_CacheHeadPushDedupsSameRunID is the focused unit-level
// proof that cacheHeadPush itself drops a same-RunID push when the ring
// head already carries it. Belts-and-braces companion to the concurrent
// integration test above so a regression that breaks dedup is pinned at
// the smallest possible call surface.
func TestRunStore_CacheHeadPushDedupsSameRunID(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	store := newRunStore(storePath, 10, 24*time.Hour)

	jobID := "abcd1234abcd5678"
	// Force-warm an empty entry so cacheHeadPush is allowed to push.
	store.warmCache(jobID)

	s1 := CronRunSummary{RunID: "0000000000000001", JobID: jobID, State: RunStateSucceeded, StartedAt: time.Now()}
	store.cacheHeadPush(jobID, s1)
	store.cacheHeadPush(jobID, s1) // dup; must be skipped
	store.cacheHeadPush(jobID, s1) // dup; must be skipped

	got := store.Recent(jobID, 10)
	if len(got) != 1 {
		t.Fatalf("ring after 3× same-RunID push: want 1 entry, got %d", len(got))
	}
	if got[0].RunID != "0000000000000001" {
		t.Errorf("ring head RunID: got %q", got[0].RunID)
	}

	// A different RunID must still push normally.
	s2 := CronRunSummary{RunID: "0000000000000002", JobID: jobID, State: RunStateSucceeded, StartedAt: time.Now()}
	store.cacheHeadPush(jobID, s2)
	got = store.Recent(jobID, 10)
	if len(got) != 2 {
		t.Fatalf("ring after distinct push: want 2 entries, got %d", len(got))
	}
	if got[0].RunID != "0000000000000002" {
		t.Errorf("newest after second push: got %q want s2", got[0].RunID)
	}
}

// TestRunStore_CacheHeadPushDedupIndexEviction pins the #1517 O(1) RunID dedup
// index (recentCacheEntry.runIDs): the index must stay in lockstep with the
// ring as entries are evicted. When the ring is full and the oldest entry is
// pushed out, its RunID must be removed from the index — otherwise a later
// re-appearance of that same RunID (e.g. via a fresh warmCache reseed picking
// it back up, or a wrapped ring re-push) would be wrongly deduped and silently
// dropped, leaving the ring shorter than the disk truth.
func TestRunStore_CacheHeadPushDedupIndexEviction(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	const keep = 3
	store := newRunStore(storePath, keep, 24*time.Hour)

	jobID := "abcd1234abcd5678"
	store.warmCache(jobID) // force-warm an empty ring so pushes are allowed

	push := func(n uint64) {
		store.cacheHeadPush(jobID, CronRunSummary{
			RunID:     fmt.Sprintf("%016x", n),
			JobID:     jobID,
			State:     RunStateSucceeded,
			StartedAt: time.Now(),
		})
	}

	// Fill and overflow the ring: RunIDs 1,2,3 fill it; 4,5 evict 1 and 2.
	for n := uint64(1); n <= 5; n++ {
		push(n)
	}

	// The dedup index must mirror the live ring exactly: {3,4,5}.
	v, _ := store.recentCache.Load(jobID)
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	idxLen := len(entry.runIDs)
	_, has3 := entry.runIDs[fmt.Sprintf("%016x", uint64(3))]
	_, has1 := entry.runIDs[fmt.Sprintf("%016x", uint64(1))]
	entry.mu.Unlock()
	if idxLen != keep {
		t.Fatalf("dedup index size = %d, want %d (must track ring, not grow unbounded)", idxLen, keep)
	}
	if !has3 {
		t.Errorf("dedup index missing live RunID 3")
	}
	if has1 {
		t.Errorf("dedup index still holds evicted RunID 1 — eviction did not delete it")
	}

	// Re-pushing an EVICTED RunID must succeed (not be deduped): it is no
	// longer in the ring, so it is a legitimately new head.
	push(1)
	got := store.Recent(jobID, keep)
	if len(got) != keep {
		t.Fatalf("ring after re-pushing evicted RunID: want %d entries, got %d", keep, len(got))
	}
	if got[0].RunID != fmt.Sprintf("%016x", uint64(1)) {
		t.Errorf("re-pushed evicted RunID 1 was dropped; newest=%q", got[0].RunID)
	}

	// Re-pushing a still-LIVE RunID must still be deduped (no growth, no dup).
	before := store.Recent(jobID, keep)
	push(4) // 4 is still in the ring {4,5,1}
	after := store.Recent(jobID, keep)
	if len(after) != len(before) {
		t.Errorf("re-pushing live RunID 4 changed ring length %d -> %d", len(before), len(after))
	}
}
