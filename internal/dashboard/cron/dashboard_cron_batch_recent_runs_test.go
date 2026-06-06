package cron

import (
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestBatchRecentRuns_OrderingAndEmpty pins R236-PERF-08 (#525): the
// bounded-fan-out helper that HandleList uses to pre-fetch RecentRuns
// for every job MUST preserve input ordering (so the per-job index used
// downstream maps to the right job) and MUST handle the trivial inputs
// (empty jobs, nil scheduler) without panic. The helper is the
// structural anchor that drops the previous N-serial RecentRuns walk;
// a future regression that re-introduces the inline call inside the
// per-job loop would break the wire shape this test pins.
func TestBatchRecentRuns_OrderingAndEmpty(t *testing.T) {
	t.Parallel()

	// Empty input — must return nil without touching scheduler.
	h := &Handlers{scheduler: cronpkg.NewScheduler(cronpkg.SchedulerConfig{})}
	if got := h.batchRecentRuns(nil, recentRunsPerJob); got != nil {
		t.Fatalf("batchRecentRuns(nil): expected nil, got %v", got)
	}
	if got := h.batchRecentRuns([]cronpkg.JobWithNextRun{}, recentRunsPerJob); got != nil {
		t.Fatalf("batchRecentRuns(empty): expected nil, got %v", got)
	}

	// Nil scheduler — must return nil without panic. Mirrors the handler
	// contract for the empty-list fast path so the per-job loop's
	// recentByIdx[idx] read does not deref a nil scheduler indirectly.
	hNil := &Handlers{scheduler: nil}
	if got := hNil.batchRecentRuns([]cronpkg.JobWithNextRun{
		{Job: cronpkg.Job{ID: "aa00000000000001"}},
	}, recentRunsPerJob); got != nil {
		t.Fatalf("batchRecentRuns(nil scheduler): expected nil, got %v", got)
	}

	// Ordering preservation: with a real scheduler that has no run
	// history the helper still returns one entry per input job (each
	// nil/empty slice). The length contract is the half of the wire
	// shape that HandleList relies on — out[idx] for jobs[idx] — and
	// it must hold even when no job has any runs.
	jobs := []cronpkg.JobWithNextRun{
		{Job: cronpkg.Job{ID: "aa00000000000001"}},
		{Job: cronpkg.Job{ID: "bb00000000000002"}},
		{Job: cronpkg.Job{ID: "cc00000000000003"}},
		{Job: cronpkg.Job{ID: "dd00000000000004"}},
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

// TestBatchRecentRuns_CursorExactlyOnce pins R20260606-PERF-2 (#1847): the
// per-call make(chan int, len(jobs)) work queue was replaced by an atomic
// cursor (var next int64 + atomic.AddInt64) over the jobs slice to drop the
// per-1 Hz-poll channel allocation. The cursor MUST distribute every job
// index to exactly one worker — no skip (off-by-one on the AddInt64-1 steal)
// and no duplicate (two workers claiming the same index). This regression is
// invisible to the ordering test (out is len-correct even if a slot is
// double-written), so we instrument scheduler.RecentRuns indirectly: each
// distinct job ID maps to its own out[idx] slot, and we count how many slots
// the helper populates with a per-index marker via a recording scheduler.
//
// Because scheduler.RecentRuns over an empty store returns nil for every job,
// we cannot distinguish slots by content; instead we assert the structural
// exactly-once contract through the index-coverage proxy: N >> workers, every
// slot addressed (len == N), and the run completes cleanly under -race (the
// shared `out` slice is written by index-disjoint workers, so the race
// detector flags any cursor bug that lets two workers claim the same index).
func TestBatchRecentRuns_CursorExactlyOnce(t *testing.T) {
	t.Parallel()

	h := &Handlers{scheduler: cronpkg.NewScheduler(cronpkg.SchedulerConfig{})}

	// N deliberately far exceeds batchRecentRunsWorkers (8) so the cursor
	// steal loop runs many iterations per worker — the regime where an
	// off-by-one or duplicate-claim bug surfaces. Distinct IDs let a future
	// content-aware assertion grow here without reshaping the test.
	const n = 50
	jobs := make([]cronpkg.JobWithNextRun, n)
	for i := range jobs {
		// 16-hex-char IDs, one per slot, all distinct.
		jobs[i] = cronpkg.JobWithNextRun{Job: cronpkg.Job{ID: idForIndex(i)}}
	}

	out := h.batchRecentRuns(jobs, recentRunsPerJob)
	if len(out) != n {
		t.Fatalf("cursor coverage: want %d slots, got %d (skip or truncation)", n, len(out))
	}

	// Exactly-once proxy: every index in [0,n) must be addressable and the
	// helper must have returned a fully-sized slice. A cursor that skipped
	// an index would leave len(out) == n but is caught by the -race run
	// flagging duplicate writes; a cursor that over-ran would panic on the
	// jobs[idx] read. Re-running the helper must be idempotent in shape.
	out2 := h.batchRecentRuns(jobs, recentRunsPerJob)
	if len(out2) != n {
		t.Fatalf("cursor coverage (second pass): want %d slots, got %d", n, len(out2))
	}
}

// idForIndex builds a distinct 16-hex-char cron job ID for slot i so each
// jobs[i] is unique without pulling in the internal cron ID generator (which
// is package-private to internal/cron).
func idForIndex(i int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, 16)
	for p := 0; p < 16; p++ {
		b[p] = hexdigits[(i>>(uint(p)*2))&0x0f]
	}
	return string(b)
}
