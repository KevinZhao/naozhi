package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// hexJobID returns a 16-char lowercase-hex job ID derived from i so that
// runStore.IsValidID accepts it and each index maps to a distinct ID.
func hexJobID(i int) string {
	return fmt.Sprintf("%016x", 0x100+i)
}

// TestBatchRecentRuns_CompletenessMapping pins #1847: after the work
// queue swapped from a per-call buffered channel to a shared atomic
// index, the fan-out MUST still claim every index 0..len-1 exactly once
// and write out[idx] for jobs[idx] with no nil gaps, duplicates, or
// overwrites. Job count (20) exceeds batchRecentRunsWorkers (8) so every
// worker walks past its first claim and the `idx >= len(jobs)` exit must
// fire for all of them (wg.Wait returns -> no goroutine leak). Each job i
// is seeded with a distinct run count (min(i+1, recentRunsPerJob)) so the
// returned per-job length is a fingerprint that detects any misrouting.
//
// Run with -race: this is the primary guard for the concurrent index
// claim (next.Add) and the parallel out[idx] writes.
func TestBatchRecentRuns_CompletenessMapping(t *testing.T) {
	t.Parallel()

	const nJobs = 20
	if nJobs <= batchRecentRunsWorkers {
		t.Fatalf("test setup: nJobs (%d) must exceed batchRecentRunsWorkers (%d) to force index reuse", nJobs, batchRecentRunsWorkers)
	}

	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{StorePath: storePath})

	jobs := make([]cronpkg.JobWithNextRun, nJobs)
	wantLen := make([]int, nJobs)
	for i := 0; i < nJobs; i++ {
		id := hexJobID(i)
		jobs[i] = cronpkg.JobWithNextRun{Job: cronpkg.Job{ID: id}}
		// i+1 runs, but RecentRuns caps the read at recentRunsPerJob.
		count := i + 1
		seedRunsFor(t, storePath, id, count)
		if count > recentRunsPerJob {
			count = recentRunsPerJob
		}
		wantLen[i] = count
	}

	h := &Handlers{scheduler: sched}
	out := h.batchRecentRuns(jobs, recentRunsPerJob)

	if len(out) != nJobs {
		t.Fatalf("len(out) = %d, want %d", len(out), nJobs)
	}
	for i := 0; i < nJobs; i++ {
		if len(out[i]) != wantLen[i] {
			t.Fatalf("out[%d]: len = %d, want %d (index %d misrouted or nil gap)", i, len(out[i]), wantLen[i], i)
		}
		// Every returned summary must carry the JobID of jobs[i] — proves
		// out[idx] was written from RecentRuns(jobs[idx].Job.ID), i.e. no
		// cross-index overwrite.
		for k, sum := range out[i] {
			if sum.JobID != jobs[i].Job.ID {
				t.Fatalf("out[%d][%d].JobID = %q, want %q (cross-index write)", i, k, sum.JobID, jobs[i].Job.ID)
			}
		}
	}
}

// seedRunsFor is seedRuns with a per-job-unique run ID prefix so run files
// never collide across jobs sharing the runs/ root.
func seedRunsFor(t *testing.T, storePath, jobID string, count int) {
	t.Helper()
	runsDir := filepath.Join(filepath.Dir(storePath), "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs %s: %v", jobID, err)
	}
	now := time.Now().UTC()
	for k := 0; k < count; k++ {
		runID := fmt.Sprintf("%s%04x", jobID[:12], k)
		started := now.Add(time.Duration(k) * time.Second)
		rec := cronpkg.CronRun{
			RunID:      runID,
			JobID:      jobID,
			State:      cronpkg.RunStateSucceeded,
			Trigger:    cronpkg.TriggerScheduled,
			StartedAt:  started,
			EndedAt:    started.Add(time.Second),
			DurationMS: 1000,
		}
		blob, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal run: %v", err)
		}
		path := filepath.Join(runsDir, runID+".json")
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			t.Fatalf("write run %s: %v", path, err)
		}
		mt := started
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}
