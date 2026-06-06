package cron

import (
	"context"
	"testing"
	"time"
)

// TestRunStore_TrimAll_PrewarmsRecentCache pins R250-PERF-9 (#1112): the
// cold-start GC pass (trimAllCtx, invoked from Scheduler.Start's background
// goroutine) must leave each surviving job's recentCache entry WARM so the
// first dashboard RecentRuns poll after a process restart hits the cache
// instead of cold-warming serially on the request path.
//
// Setup: seed a run on disk (so the job dir exists), then force the cache
// cold. Run trimAllCtx and assert the entry is warm afterwards — without a
// cacheGet (which would itself lazily warm and mask a regression).
func TestRunStore_TrimAll_PrewarmsRecentCache(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	// Seed a run so runs/<jobID>/ exists with a parseable record.
	s.Append(makeRun(jobID, time.Now().Add(-1*time.Hour)))

	// Force the cache cold so trimAllCtx's pre-warm is the only thing that
	// can flip it back to warm.
	s.cacheInvalidate(jobID)
	if v, ok := s.recentCache.Load(jobID); ok {
		entry := v.(*recentCacheEntry)
		entry.mu.Lock()
		warm := entry.warm
		entry.mu.Unlock()
		if warm {
			t.Fatal("precondition: cache must be cold after cacheInvalidate")
		}
	}

	s.trimAllCtx(context.Background(), time.Now())

	v, ok := s.recentCache.Load(jobID)
	if !ok {
		t.Fatal("cold-start trim must have pre-warmed (created) the cache entry")
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	warm := entry.warm
	count := entry.count
	entry.mu.Unlock()
	if !warm {
		t.Fatal("cold-start trim must leave the recentCache entry warm (#1112)")
	}
	if count == 0 {
		t.Fatal("pre-warmed cache should hold the seeded run row")
	}
}
