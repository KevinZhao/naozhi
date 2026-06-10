package cron

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRunStore_SkipAppendTrim_RLockDoesNotBlockReaders pins R20260610-GO-004:
// skipAppendTrim is a pure reader of entry state (warm / count / ringRead) and
// its caller already holds jobLock(jobID), which serialises it against every
// entry writer (cacheHeadPush, warmCacheLocked, cacheTrimAfterDisk). It must
// therefore take entry.mu in READ mode so concurrent dashboard cacheGet /
// cacheGetBefore RLock readers are never blocked behind it.
//
// The test simulates a dashboard reader holding entry.mu.RLock and asserts
// skipAppendTrim (under jobLock, per its caller contract) completes anyway.
// With the pre-fix exclusive Lock this blocks until the reader releases, so
// the regression manifests as a timeout failure instead of a hang.
func TestRunStore_SkipAppendTrim_RLockDoesNotBlockReaders(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store := newRunStore(filepath.Join(tmp, "cron_jobs.json"), 32, 24*time.Hour)
	if store.disabled {
		t.Fatal("store disabled")
	}

	jobID := "0a0bdeadbeef5678"
	now := time.Now()
	// Seed one run so the windowSafe branch exercises ringRead under the lock.
	store.Append(&CronRun{
		JobID:     jobID,
		RunID:     "0000000000000001",
		State:     RunStateSucceeded,
		StartedAt: now,
		EndedAt:   now,
	})
	if rows, ok := store.cacheGet(jobID, 32); !ok || len(rows) != 1 {
		t.Fatalf("cacheGet warm-up failed: ok=%v len=%d", ok, len(rows))
	}

	v, ok := store.recentCache.Load(jobID)
	if !ok {
		t.Fatal("recentCache entry missing after Append")
	}
	entry := v.(*recentCacheEntry)

	// Simulate a dashboard reader pinning the entry in read mode.
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	done := make(chan bool, 1)
	go func() {
		lock := store.jobLock(jobID) // caller contract: hold jobLock
		lock.Lock()
		defer lock.Unlock()
		done <- store.skipAppendTrim(jobID, time.Now())
	}()

	select {
	case skip := <-done:
		// count=1 with keep=32 headroom and a fresh row inside keepWindow:
		// both proofs hold, so the skip decision itself must be true.
		if !skip {
			t.Error("skipAppendTrim returned false; cache-headroom proofs should hold")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("skipAppendTrim blocked behind a concurrent RLock reader; entry.mu must be taken in read mode (R20260610-GO-004)")
	}
}

// TestRunStore_SkipAppendTrim_RaceWithCacheGet drives skipAppendTrim (under
// jobLock) concurrently with cacheGet RLock readers on the same entry so the
// race detector proves the RLock-only read path never aliases reader state.
// Complements runstore_rwmutex_reader_race_test.go, which exercises the full
// Append writer path rather than skipAppendTrim in isolation.
func TestRunStore_SkipAppendTrim_RaceWithCacheGet(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	const keep = 32
	store := newRunStore(filepath.Join(tmp, "cron_jobs.json"), keep, 24*time.Hour)
	if store.disabled {
		t.Fatal("store disabled")
	}

	jobID := "0c0ffeedfacecafe"
	now := time.Now()
	store.Append(&CronRun{
		JobID:     jobID,
		RunID:     "0000000000000002",
		State:     RunStateSucceeded,
		StartedAt: now,
		EndedAt:   now,
	})

	const iters = 2000
	const readers = 4

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		lock := store.jobLock(jobID)
		for i := 0; i < iters; i++ {
			lock.Lock()
			store.skipAppendTrim(jobID, time.Now())
			lock.Unlock()
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if rows, ok := store.cacheGet(jobID, keep); ok && len(rows) > keep {
					t.Errorf("cacheGet returned %d rows > keep %d", len(rows), keep)
				}
			}
		}()
	}

	wg.Wait()
}
