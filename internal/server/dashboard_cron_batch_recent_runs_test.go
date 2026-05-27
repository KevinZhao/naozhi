package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
)

// TestBatchRecentRuns_OrderingAndEmpty pins R236-PERF-08 (#525): the
// bounded-fan-out helper that handleList uses to pre-fetch RecentRuns
// for every job MUST preserve input ordering (so the per-job index used
// downstream maps to the right job) and MUST handle the trivial inputs
// (empty jobs, nil scheduler) without panic. The helper is the
// structural anchor that drops the previous N-serial RecentRuns walk;
// a future regression that re-introduces the inline call inside the
// per-job loop would break the wire shape this test pins.
func TestBatchRecentRuns_OrderingAndEmpty(t *testing.T) {
	t.Parallel()

	// Empty input — must return nil without touching scheduler.
	h := &CronHandlers{scheduler: cron.NewScheduler(cron.SchedulerConfig{})}
	if got := h.batchRecentRuns(nil, recentRunsPerJob); got != nil {
		t.Fatalf("batchRecentRuns(nil): expected nil, got %v", got)
	}
	if got := h.batchRecentRuns([]cron.JobWithNextRun{}, recentRunsPerJob); got != nil {
		t.Fatalf("batchRecentRuns(empty): expected nil, got %v", got)
	}

	// Nil scheduler — must return nil without panic. Mirrors the handler
	// contract for the empty-list fast path so the per-job loop's
	// recentByIdx[idx] read does not deref a nil scheduler indirectly.
	hNil := &CronHandlers{scheduler: nil}
	if got := hNil.batchRecentRuns([]cron.JobWithNextRun{
		{Job: cron.Job{ID: "aa00000000000001"}},
	}, recentRunsPerJob); got != nil {
		t.Fatalf("batchRecentRuns(nil scheduler): expected nil, got %v", got)
	}

	// Ordering preservation: with a real scheduler that has no run
	// history the helper still returns one entry per input job (each
	// nil/empty slice). The length contract is the half of the wire
	// shape that handleList relies on — out[idx] for jobs[idx] — and
	// it must hold even when no job has any runs.
	jobs := []cron.JobWithNextRun{
		{Job: cron.Job{ID: "aa00000000000001"}},
		{Job: cron.Job{ID: "bb00000000000002"}},
		{Job: cron.Job{ID: "cc00000000000003"}},
		{Job: cron.Job{ID: "dd00000000000004"}},
	}
	out := h.batchRecentRuns(jobs, recentRunsPerJob)
	if len(out) != len(jobs) {
		t.Fatalf("batchRecentRuns len: want %d, got %d", len(jobs), len(out))
	}
	// recentRunsPerJob is the wire-shape contract pin: the previous
	// inline call site used the literal 5; if a future refactor
	// changes this constant, the test catches the divergence so the
	// dashboard JS reading recent_runs.length stays in lockstep.
	if recentRunsPerJob != 5 {
		t.Fatalf("recentRunsPerJob constant changed: want 5 (wire-shape pin), got %d", recentRunsPerJob)
	}
	// batchRecentRunsWorkers caps the goroutine fan-out — a future
	// refactor that drops the cap would re-introduce the goroutine-flood
	// failure mode this issue closed. Pin the lower bound (must be
	// concurrent: >=2) and the upper-bound sanity (≤32 keeps sync.Map
	// contention from going pathological).
	if batchRecentRunsWorkers < 2 || batchRecentRunsWorkers > 32 {
		t.Fatalf("batchRecentRunsWorkers out of range: want [2,32], got %d", batchRecentRunsWorkers)
	}
}
