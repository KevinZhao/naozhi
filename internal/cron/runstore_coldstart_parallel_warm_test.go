package cron

import (
	"context"
	"testing"
	"time"
)

// TestRunStore_TrimAll_ParallelWarm_AllJobs pins R20260601-PERF-8 (#1550):
// the cold-start GC pass must warm EVERY surviving job's recentCache, not
// just the first. The serial loop was replaced by a bounded goroutine pool
// (warmJobsParallel); this asserts the fan-out still warms all jobs.
func TestRunStore_TrimAll_ParallelWarm_AllJobs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)

	const nJobs = 20
	jobIDs := make([]string, 0, nJobs)
	for i := 0; i < nJobs; i++ {
		jobID := mustGenerateID()
		jobIDs = append(jobIDs, jobID)
		s.Append(makeRun(jobID, time.Now().Add(-time.Duration(i+1)*time.Minute)))
		s.cacheInvalidate(jobID)
	}

	s.trimAllCtx(context.Background(), time.Now())

	for _, jobID := range jobIDs {
		v, ok := s.recentCache.Load(jobID)
		if !ok {
			t.Fatalf("job %s: cold-start trim must have created the cache entry", jobID)
		}
		entry := v.(*recentCacheEntry)
		entry.mu.Lock()
		warm := entry.warm
		count := entry.count
		entry.mu.Unlock()
		if !warm {
			t.Fatalf("job %s: parallel warm must leave the entry warm (#1550)", jobID)
		}
		if count == 0 {
			t.Fatalf("job %s: warmed cache should hold the seeded run row", jobID)
		}
	}
}

// TestRunStore_WarmJobsParallel_CancelledCtx pins that a cancelled ctx
// short-circuits the parallel warm so Scheduler.Stop stays prompt: no job
// should be warmed once the ctx is already done before the call.
func TestRunStore_WarmJobsParallel_CancelledCtx(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)

	const nJobs = 10
	jobIDs := make([]string, 0, nJobs)
	for i := 0; i < nJobs; i++ {
		jobID := mustGenerateID()
		jobIDs = append(jobIDs, jobID)
		s.Append(makeRun(jobID, time.Now().Add(-time.Duration(i+1)*time.Minute)))
		s.cacheInvalidate(jobID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	s.warmJobsParallel(ctx, jobIDs)

	warmed := 0
	for _, jobID := range jobIDs {
		v, ok := s.recentCache.Load(jobID)
		if !ok {
			continue
		}
		entry := v.(*recentCacheEntry)
		entry.mu.Lock()
		if entry.warm {
			warmed++
		}
		entry.mu.Unlock()
	}
	if warmed != 0 {
		t.Fatalf("cancelled ctx must short-circuit all warms, got %d warmed", warmed)
	}
}

// TestRunStore_WarmJobsParallel_Empty is a defensive no-op guard.
func TestRunStore_WarmJobsParallel_Empty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.warmJobsParallel(context.Background(), nil)
	s.warmJobsParallel(context.Background(), []string{})
}
