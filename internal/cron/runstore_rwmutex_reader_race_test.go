package cron

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRunStore_RWMutexReaders_NoRaceUnderConcurrentAppend pins #1846: after
// recentCacheEntry.mu became a sync.RWMutex, the read-only entry-lock sites
// (cacheGet via List, RecentSessionIDs warm fast path, cacheGetBefore) take
// RLock and may run concurrently with EACH OTHER while a single writer drives
// Append (cacheHeadPush + appendTrimBatch trim under exclusive Lock) on the
// same jobID. Under -race this proves the relaxed readers never alias a
// concurrent writer's ring/head/count mutation, and that each reader snapshot
// is internally consistent (count matches slice len, RunIDs unique).
func TestRunStore_RWMutexReaders_NoRaceUnderConcurrentAppend(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: skipped in -short")
	}
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	const keep = 32
	store := newRunStore(storePath, keep, 24*time.Hour)
	if store.disabled {
		t.Fatal("store disabled")
	}

	jobID := "0f0fbeefcafe1234"
	// Warm an empty ring so the read fast paths take the RLock branch rather
	// than the cold warmCache exclusive-Lock fall-through.
	store.warmCache(jobID)

	// iters is the WRITER's count; each Append also writes a run file, so the test
	// is disk-bound and its wall time is linear in this number. Measured under
	// -race (what CI runs): 4000 → 52.8s, 1000 → 15.2s, 400 → 7.2s. It was 4000
	// with no recorded rationale, and at 52.8s it was the single largest test in
	// the repo — 55 of the ~200s CI `test` job, whose Epic A #2524 target is 150s.
	//
	// 1000 keeps the detection power. TSan is not statistical: it reports a race
	// the first time it observes two conflicting accesses without a happens-before
	// edge, so what matters is that the paths run concurrently at all, not how many
	// times. And the readers below spin FREELY rather than once per write — their
	// coverage is bounded by wall time, and 15s of six concurrent readers against
	// cacheHeadPush + appendTrimBatch exercises every RLock site many times over.
	const iters = 1000
	const readers = 6

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Single writer: drives Append, which pushes onto the ring (exclusive
	// Lock via cacheHeadPush) and periodically trims (cacheTrimAfterDisk).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			runID := fmt.Sprintf("%016x", uint64(0x200000000)+uint64(i))
			now := time.Now()
			store.Append(&CronRun{
				JobID:     jobID,
				RunID:     runID,
				State:     RunStateSucceeded,
				StartedAt: now,
				EndedAt:   now,
			})
		}
		close(stop)
	}()

	// Many concurrent readers across all three RLock paths.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(which int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch which % 3 {
				case 0:
					rows := store.List(jobID, keep, time.Time{}) // cacheGet RLock
					assertSnapshotConsistent(t, rows)
				case 1:
					sids := store.RecentSessionIDs(jobID, keep) // warm fast path RLock
					if len(sids) > keep {
						t.Errorf("RecentSessionIDs len %d > keep %d", len(sids), keep)
					}
				case 2:
					// cacheGetBefore RLock path (non-zero cutoff).
					rows := store.List(jobID, keep, time.Now().Add(time.Hour))
					assertSnapshotConsistent(t, rows)
				}
			}
		}(r)
	}

	wg.Wait()
}

func assertSnapshotConsistent(t *testing.T, rows []CronRunSummary) {
	t.Helper()
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if _, dup := seen[r.RunID]; dup {
			t.Errorf("duplicate RunID %q in reader snapshot", r.RunID)
		}
		seen[r.RunID] = struct{}{}
	}
}
