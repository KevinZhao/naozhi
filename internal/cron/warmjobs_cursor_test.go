package cron

import (
	"context"
	"testing"
	"time"
)

// TestRunStore_WarmJobsParallel_CursorCoversAllJobs pins R20260603-PERF-5: the
// atomic-cursor work distribution must warm every job exactly once even when
// there are far more jobs than workers (diskDecodeWorkers=8), so each worker
// steals multiple indices. A skipped or double-claimed index would leave a job
// cold or race the per-entry warm. We use 50 jobs (>> 8 workers) to force many
// FetchAdd steals per worker.
func TestRunStore_WarmJobsParallel_CursorCoversAllJobs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)

	const nJobs = 50
	jobIDs := make([]string, 0, nJobs)
	for i := 0; i < nJobs; i++ {
		jobID := mustGenerateID()
		jobIDs = append(jobIDs, jobID)
		s.Append(makeRun(jobID, time.Now().Add(-time.Duration(i+1)*time.Minute)))
		s.cacheInvalidate(jobID)
	}

	s.warmJobsParallel(context.Background(), jobIDs)

	for _, jobID := range jobIDs {
		v, ok := s.recentCache.Load(jobID)
		if !ok {
			t.Fatalf("job %s: cursor missed a job (no cache entry)", jobID)
		}
		entry := v.(*recentCacheEntry)
		entry.mu.Lock()
		warm := entry.warm
		count := entry.count
		entry.mu.Unlock()
		if !warm {
			t.Fatalf("job %s: cursor left entry cold", jobID)
		}
		if count == 0 {
			t.Fatalf("job %s: warmed cache missing seeded run", jobID)
		}
	}
}
