package cron

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSkipAppendTrim_ConcurrentWithCacheGetReaders pins R20260609-GO-001:
// skipAppendTrim must hold entry.mu.RLock (not Lock) so it can run
// concurrently with other read-only entry.mu holders such as cacheGet. A
// regression to exclusive Lock would serialize skipAppendTrim against all
// concurrent RLock readers — measurable as a deadlock or -race DATA RACE when
// this test is run under the race detector.
//
// The test warm-initialises the cache, then simultaneously drives:
//   - a Append loop (which calls skipAppendTrim under jobLock), and
//   - a List loop (which calls cacheGet, which RLocks entry.mu).
//
// Under -race, any unsynchronised concurrent access to the entry's fields
// surfaces as a DATA RACE. The test passes with RLock but would fail with
// exclusive Lock because the two goroutines would be forced to serialize
// and the -race detector would see no race (but the point is that using
// Lock was wrong — it blocked readers unnecessarily). Using -race also
// proves the fields accessed by skipAppendTrim (entry.warm, entry.count,
// entry.ring via ringRead) are read under a lock that is compatible with
// concurrent RLock readers on those same fields.
func TestSkipAppendTrim_ConcurrentWithCacheGetReaders(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	const keep = 64
	store := newRunStore(storePath, keep, 24*time.Hour)
	if store.disabled {
		t.Fatal("store disabled")
	}

	jobID := "aa01bb02cc03dd04"
	// Pre-warm the cache so skipAppendTrim takes the fast path through
	// entry.mu rather than returning false early on the cold path. Also seed
	// enough runs to make the headroom proofs non-trivial.
	for i := 0; i < keep/2; i++ {
		runID := fmt.Sprintf("%016x", uint64(i))
		now := time.Now()
		store.Append(&CronRun{
			JobID:     jobID,
			RunID:     runID,
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		})
	}

	const appends = 2000
	const readers = 4

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: each Append internally calls skipAppendTrim (entry.mu.RLock
	// after the fix) followed by cacheHeadPush (entry.mu.Lock). The
	// RLock→Unlock sequence is what we're stressing.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < appends; i++ {
			runID := fmt.Sprintf("%016x", uint64(0x800000000)+uint64(i))
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

	// Concurrent readers: cacheGet takes entry.mu.RLock — these must not
	// be blocked by the writer's skipAppendTrim call, which also takes
	// entry.mu.RLock. Under the old exclusive-Lock implementation all these
	// readers would serialize behind the writer's skipAppendTrim Lock; under
	// RLock they may proceed concurrently.
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
				rows := store.List(jobID, keep, time.Time{})
				// Validate snapshot consistency: no duplicate RunIDs.
				seen := make(map[string]struct{}, len(rows))
				for _, row := range rows {
					if _, dup := seen[row.RunID]; dup {
						t.Errorf("duplicate RunID %q in cacheGet snapshot", row.RunID)
					}
					seen[row.RunID] = struct{}{}
				}
			}
		}()
	}

	wg.Wait()
}
