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

// TestRunStore_DecodeRunsParallel_OrderPreserved pins R20260602190132-PERF-9
// (#1610): after replacing the per-call make(chan int, n) work queue with a
// shared atomic cursor, the parallel decode path must still return summaries
// in newest-first order and must read every candidate (no dropped/duplicated
// index). We seed > diskDecodeParallelThreshold runs with strictly
// decreasing StartedAt timestamps, drop the cache so List hits disk via
// decodeRunsParallel, and assert the result is the full set in newest-first
// order. Run under -race this also guards the atomic distribution against
// data races on the slots slice.
func TestRunStore_DecodeRunsParallel_OrderPreserved(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	// n must exceed diskDecodeParallelThreshold (16) so List routes into
	// decodeRunsParallel, and exceed diskDecodeWorkers (8) so multiple slots
	// fall to each worker through the atomic cursor.
	const n = 64
	base := time.Now()
	wantOrder := make([]string, n)
	for i := 0; i < n; i++ {
		// newest first: i=0 is the most recent. StartedAt drives the
		// scanSortedRunDir mtime/order, so space them out clearly.
		startedAt := base.Add(-time.Duration(i) * time.Minute)
		run := makeRun(jobID, startedAt)
		s.Append(run)
		wantOrder[i] = run.RunID
	}

	// Force the disk path: clear the in-memory cache so List re-reads + decodes.
	s.cacheInvalidate(jobID)

	got := s.List(jobID, 200, time.Time{})
	if len(got) != n {
		t.Fatalf("List len=%d want %d (atomic cursor must read every candidate)", len(got), n)
	}
	for i := 0; i < n; i++ {
		if got[i].RunID != wantOrder[i] {
			t.Fatalf("order mismatch at %d: got RunID %s want %s (newest-first must survive atomic distribution)",
				i, got[i].RunID, wantOrder[i])
		}
		if !got[i].StartedAt.Equal(base.Add(-time.Duration(i) * time.Minute)) {
			t.Fatalf("StartedAt mismatch at %d: got %s", i, got[i].StartedAt)
		}
	}
}
