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

// TestRunStore_DecodeRunsParallel_AllSlotsConsumed pins R20260602190132-PERF-9
// (#1610): after replacing the per-call make(chan int, n) work queue with a
// shared atomic cursor, every candidate slot must be visited exactly once
// regardless of how many workers race the cursor. We drive a wide fan-out
// (n far above diskDecodeWorkers) through decodeRunsParallel directly and
// assert every seeded run is decoded (none dropped, none read twice). Run
// under -race this also guards the atomic distribution against data races on
// the shared slots slice. The newest-first ordering / corrupt-skip / limit
// behaviours are covered by the diskListNewestFirst parallel tests in
// diskdecode_parallel_test.go.
func TestRunStore_DecodeRunsParallel_AllSlotsConsumed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	// n must exceed diskDecodeWorkers (8) by a wide margin so many slots fall
	// to each worker through the atomic cursor — the regime the old per-call
	// channel handled and the new cursor must match.
	const n = 100
	base := time.Now().Add(-time.Hour)
	wantIDs := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		run := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		s.Append(run)
		wantIDs[run.RunID] = true
	}

	items, _, err := s.scanSortedRunDir(jobID)
	if err != nil {
		t.Fatalf("scanSortedRunDir: %v", err)
	}
	if len(items) != n {
		t.Fatalf("scanSortedRunDir len=%d want %d", len(items), n)
	}

	rows, corrupt := s.decodeRunsParallel(items, n)
	if corrupt != 0 {
		t.Fatalf("corruptCount=%d want 0", corrupt)
	}
	if len(rows) != n {
		t.Fatalf("decoded %d rows want %d (atomic cursor must visit every slot once)", len(rows), n)
	}
	gotIDs := make(map[string]bool, n)
	for _, r := range rows {
		if gotIDs[r.RunID] {
			t.Fatalf("RunID %s decoded twice — cursor handed the same index out twice", r.RunID)
		}
		gotIDs[r.RunID] = true
		if !wantIDs[r.RunID] {
			t.Fatalf("decoded unexpected RunID %s", r.RunID)
		}
	}
	if len(gotIDs) != n {
		t.Fatalf("distinct decoded IDs=%d want %d (some slots skipped)", len(gotIDs), n)
	}
}
