package cron

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFinishRun_DeleteRaceNoOrphanRunsDir pins #2058: finishRun's terminal
// write is two-step and non-atomic. recordTerminalResult confirms the job
// exists, bumps RunCounters, persists cron_jobs.json, then RELEASES s.mu and
// returns jobPersistOK=true. appendRun then writes the physically-separate
// runs/<jobID>/ store, which never takes s.mu.
//
// A DeleteJobByID landing in that window drops the job from s.jobs AND runs
// runStore.DeleteJob → RemoveAll(runs/<jobID>). Without the s.jobs re-check
// added before appendRun (#2058), the stale snapshot's appendRun →
// ensureJobDir would MkdirAll the directory back, resurrecting an orphaned
// runs/<jobID>/ subtree for a job that no longer exists in cron_jobs.json (a
// bounded disk leak that retention trimming never reclaims).
//
// #2473: the original version raced two goroutines 200× per run and hoped
// the scheduler would land the delete inside the window; on CI it
// occasionally landed the delete AFTER the re-check instead (a residual
// window finishRun documents as best-effort, tracked separately) and the
// assertion fired. This version uses the finishRunPreAppendHook seam to run
// the delete synchronously inside the exact window #2058 closed, so the
// interleaving is fixed rather than sampled. Removing the jobStillExists
// guard from finishRun makes this test fail deterministically.
func TestFinishRun_DeleteRaceNoOrphanRunsDir(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron.json")
	s := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 5}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	j := &Job{
		Schedule: "@every 1h",
		Prompt:   "ping",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true, // no live cron entry
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	jobID := j.ID
	jobRunDir := filepath.Join(dir, "runs", jobID)

	// The hook fires after recordTerminalResult has persisted the Job and
	// released s.mu, immediately before the runs/<jobID>/ existence re-check.
	// Deleting synchronously here is the interleaving #2058 closes. Set before
	// finishRun runs; nothing else touches s concurrently in this test.
	hookCalls := 0
	var deleteErr error
	s.finishRunPreAppendHook = func(gotJobID string) {
		hookCalls++
		if gotJobID != jobID {
			t.Errorf("hook job id = %q, want %q", gotJobID, jobID)
		}
		_, deleteErr = s.DeleteJobByID(jobID)
	}

	inflight := s.jobInflight(jobID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	s.finishRun(finishArgs{
		job:       j,
		runID:     "0123456789abcdef",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		sessionID: "sess-1",
		result:    "ok",
		finalizer: &runFinalizer{inflight: inflight},
	})

	if hookCalls != 1 {
		t.Fatalf("finishRunPreAppendHook calls = %d, want 1", hookCalls)
	}
	if deleteErr != nil {
		t.Fatalf("DeleteJobByID inside the window: %v", deleteErr)
	}
	if s.jobStillExists(jobID) {
		t.Fatal("job must be gone from s.jobs after DeleteJobByID")
	}
	// The delete removed runs/<jobID>/ and nothing may bring it back: not
	// the directory, and certainly not a run record for a deleted job.
	if _, err := os.Stat(jobRunDir); !errors.Is(err, fs.ErrNotExist) {
		entries, _ := os.ReadDir(jobRunDir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("orphaned runs subtree resurrected after job delete: %s exists (stat err=%v, entries=%v) (#2058)",
			jobRunDir, err, names)
	}
}
