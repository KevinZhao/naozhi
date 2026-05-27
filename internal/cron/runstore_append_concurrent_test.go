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
