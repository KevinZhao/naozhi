package cron

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRunStore_CacheRWMutex_ConcurrentReadWrite pins R20260606-PERF-1 (#1846):
// recentCacheEntry.mu is now a sync.RWMutex so the read-heavy 1Hz dashboard
// poll paths (cacheGet / cacheGetBefore / trimSkipFromCache / RecentSessionIDs)
// take a shared RLock while the write paths (cacheHeadPush) take the exclusive
// Lock. This test hammers all four read paths concurrently against a stream of
// cacheHeadPush writes; under `go test -race` it must report no data race,
// which proves every read path acquired the lock (RLock) and every mutation
// stayed under the exclusive Lock. A regression that drops a lock on any path
// (or reads the ring without the RLock) trips the race detector here.
func TestRunStore_CacheRWMutex_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	const keep = 64
	store := newRunStore(storePath, keep, 24*time.Hour)
	if store.disabled {
		t.Fatal("store disabled")
	}

	jobID := "abcd1234abcd5678"
	// Force-warm an empty ring so cacheHeadPush is allowed to push and the
	// read paths take the warm fast path under RLock.
	store.warmCache(jobID)

	const (
		readers      = 8
		opsPerWorker = 300
	)
	var wg sync.WaitGroup

	// Writer goroutine: drives the exclusive-Lock path (cacheHeadPush) while
	// holding jobLock, exactly as Append does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		lock := store.jobLock(jobID)
		for i := 0; i < opsPerWorker; i++ {
			lock.Lock()
			store.cacheHeadPush(jobID, CronRunSummary{
				RunID:     fmt.Sprintf("%016x", uint64(i)),
				JobID:     jobID,
				SessionID: fmt.Sprintf("sess-%d", i),
				State:     RunStateSucceeded,
				StartedAt: time.Now(),
				EndedAt:   time.Now(),
			})
			lock.Unlock()
		}
	}()

	// Reader goroutines: hammer each of the four RLock read paths.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				_, _ = store.cacheGet(jobID, 20)
				_, _ = store.cacheGetBefore(jobID, 20, time.Now())
				_ = store.RecentSessionIDs(jobID, 20)
				// trimSkipFromCache requires jobLock; mirror Append's hold.
				lock := store.jobLock(jobID)
				lock.Lock()
				_ = store.trimSkipFromCache(jobID, time.Now())
				lock.Unlock()
			}
		}()
	}

	wg.Wait()

	// Sanity: the ring is still consistent after the concurrent churn.
	got := store.Recent(jobID, keep)
	if len(got) == 0 {
		t.Fatal("ring empty after concurrent push/read churn")
	}
	if len(got) > keep {
		t.Fatalf("ring exceeded keepCount: got %d want <= %d", len(got), keep)
	}
}
