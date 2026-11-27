package runstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// TestRecentCacheHitsAvoidDiskIO: after a single Append warms
// the cache, subsequent Recent calls should not re-stat the runs/<jobID>/
// directory. Verified by deleting the on-disk file post-warm and confirming
// Recent still returns the run (cache served).
func TestRecentCacheHitsAvoidDiskIO(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := New(Options{StorePath: filepath.Join(tmp, "cron_jobs.json")})

	jobID := mustGenerateID()
	runID := mustGenerateRunID()
	run := &CronRun{
		RunID: runID, JobID: jobID, State: runtelemetry.RunStateSucceeded,
		StartedAt: time.Now(), EndedAt: time.Now(),
	}
	s.Append(run)

	// First Recent — warms cache from disk.
	first := s.Recent(jobID, 10)
	if len(first) != 1 || first[0].RunID != runID {
		t.Fatalf("first Recent: got %+v", first)
	}

	// Now delete the on-disk file. If cache is correctly serving, Recent
	// will still return the same record. If cache miss falls through to
	// disk every call, len would be 0.
	if err := os.Remove(filepath.Join(tmp, "runs", jobID, runID+".json")); err != nil {
		t.Fatalf("remove disk file: %v", err)
	}
	cached := s.Recent(jobID, 10)
	if len(cached) != 1 || cached[0].RunID != runID {
		t.Errorf("cache miss after disk delete; got %+v want 1 entry served from cache", cached)
	}
}

// TestAppendPushesCacheHeadInOrder: a 2nd Append should appear
// at index 0 of the Recent slice (newest first), not 1.
func TestAppendPushesCacheHeadInOrder(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := New(Options{StorePath: filepath.Join(tmp, "cron_jobs.json")})
	s.enableTrimGC = false

	jobID := mustGenerateID()
	first := mustGenerateRunID()
	second := mustGenerateRunID()
	s.Append(&CronRun{RunID: first, JobID: jobID, State: runtelemetry.RunStateSucceeded, StartedAt: time.Now().Add(-time.Hour)})
	// Warm cache via Recent before second Append.
	_ = s.Recent(jobID, 10)
	s.Append(&CronRun{RunID: second, JobID: jobID, State: runtelemetry.RunStateSucceeded, StartedAt: time.Now()})

	got := s.Recent(jobID, 10)
	if len(got) < 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].RunID != second {
		t.Errorf("newest first violated: got[0]=%q want %q", got[0].RunID, second)
	}
	if got[1].RunID != first {
		t.Errorf("got[1]=%q want %q", got[1].RunID, first)
	}
}

// TestDeleteJobInvalidatesCache: after DeleteJob, Recent
// returns nil even though the entry was previously cached.
func TestDeleteJobInvalidatesCache(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := New(Options{StorePath: filepath.Join(tmp, "cron_jobs.json")})

	jobID := mustGenerateID()
	s.Append(&CronRun{RunID: mustGenerateRunID(), JobID: jobID, State: runtelemetry.RunStateSucceeded, StartedAt: time.Now()})
	if got := s.Recent(jobID, 10); len(got) != 1 {
		t.Fatalf("warm: got %d entries, want 1", len(got))
	}

	s.DeleteJob(jobID)
	if got := s.Recent(jobID, 10); len(got) != 0 {
		t.Errorf("post-DeleteJob: cache should be empty; got %d entries", len(got))
	}
}
