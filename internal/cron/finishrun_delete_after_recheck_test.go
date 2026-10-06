package cron

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron/runstore"
)

// TestFinishRun_DeleteAfterRecheckNoOrphanRunsDir pins #2479, the residual
// window left open by #2058. finishRun's pre-write jobStillExists re-check
// only covers a DeleteJobByID that lands BEFORE it; runstore.Store.Append then
// runs ensureJobDir (MkdirAll) + WriteFileAtomic outside jobLock (#1335),
// and DeleteJob drops the jobLock entry after RemoveAll, so a delete landing
// AFTER the re-check but before the disk write cannot be serialised against
// the write at all: runs/<jobID>/<runID>.json is written back for a job that
// no longer exists in cron_jobs.json, and trimAll (which only walks known
// jobs) never reclaims the orphaned directory. CI hit this for real (issue
// #2479 quotes the TestFinishRun_DeleteRaceNoOrphanRunsDir failure).
//
// The fix is a second, post-write re-check in finishRun: when the job is
// gone after Append returned, runstore.Store.DropOrphanRun removes exactly the
// run file this finishRun wrote and rmdir's runs/<jobID>/ if that left it
// empty. This test injects the delete via runstore.Options.AppendPreWriteHook
// so the interleaving is fixed, not sampled. Removing the post-write re-check
// (or DropOrphanRun) makes it fail deterministically.
func TestFinishRun_DeleteAfterRecheckNoOrphanRunsDir(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron.json")
	s := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 5}, SchedulerDeps{})

	// The hook fires inside runstore.Store.Append: after finishRun's pre-write
	// jobStillExists re-check has already passed (#2058) and the record is
	// marshalled, immediately before ensureJobDir + WriteFileAtomic. This is
	// exactly the window #2479 describes. The store is swapped for one over the
	// same StorePath that carries the hook before Start, so no goroutine has
	// read s.runs yet; jobID is assigned below, before finishRun calls it.
	var jobID string
	hookCalls := 0
	var deleteErr error
	s.runs = runstore.New(runstore.Options{StorePath: storePath, AppendPreWriteHook: func(gotJobID string) {
		hookCalls++
		if gotJobID != jobID {
			t.Errorf("hook job id = %q, want %q", gotJobID, jobID)
		}
		_, deleteErr = s.DeleteJobByID(jobID)
	}})
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
	jobID = j.ID
	jobRunDir := filepath.Join(dir, "runs", jobID)

	inflight := s.gateForTest().jobInflight(jobID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	s.finishRun(runCtx{jobID: j.ID, runID: "0123456789abcdef", startedAt: time.Now(), trigger: TriggerScheduled, finalizer: &runFinalizer{inflight: inflight}}, runOutcome{state: RunStateSucceeded, sessionID: "sess-1", result: "ok"})

	if hookCalls != 1 {
		t.Fatalf("appendPreWriteHook calls = %d, want 1", hookCalls)
	}
	if deleteErr != nil {
		t.Fatalf("DeleteJobByID inside the window: %v", deleteErr)
	}
	if s.jobStillExists(jobID) {
		t.Fatal("job must be gone from s.tbl.jobs after DeleteJobByID")
	}
	// The delete removed runs/<jobID>/; the write that raced it must have
	// been undone by the post-write re-check, leaving neither the run record
	// nor an empty orphaned directory behind.
	if _, err := os.Stat(jobRunDir); !errors.Is(err, fs.ErrNotExist) {
		entries, _ := os.ReadDir(jobRunDir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("orphaned runs subtree resurrected after job delete: %s exists (stat err=%v, entries=%v) (#2479)",
			jobRunDir, err, names)
	}
}
