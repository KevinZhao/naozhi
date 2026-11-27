package cron

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestP1_FinishRunPersistsCronRun: a non-skipPersist terminal goes to
// runs/<jobID>/<run_id>.json and is readable via Get / List.
func TestP1_FinishRunPersistsCronRun(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: &fakeRouter{}})
	if !s.runs.Enabled() {
		t.Fatal("run store should be enabled when StorePath is set")
	}

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.putJobForTest(j)

	runID := mustGenerateRunID()
	startedAt := time.Now().Add(-5 * time.Second)
	s.finishRun(runCtx{jobID: j.ID, runID: runID, startedAt: startedAt, trigger: TriggerScheduled, snap: jobSnapshot{prompt: "do thing", workDir: "/tmp/wd", fresh: false}}, runOutcome{state: RunStateSucceeded, sessionID: "sess-AAA", result: "hello"})

	got, err := s.Run(jobID, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != RunStateSucceeded {
		t.Errorf("state: got %q want %q", got.State, RunStateSucceeded)
	}
	if got.Result != "hello" {
		t.Errorf("result: got %q", got.Result)
	}
	if got.SessionID != "sess-AAA" {
		t.Errorf("session_id: got %q", got.SessionID)
	}
	if got.Prompt != "do thing" {
		t.Errorf("prompt snapshot: got %q", got.Prompt)
	}

	// List path should also surface the run.
	rows := s.ListRuns(jobID, 10, time.Time{})
	if len(rows) != 1 || rows[0].RunID != runID {
		t.Fatalf("ListRuns: got %+v", rows)
	}
}

// TestP1_FinishRunSkipPersistDoesNotWriteHistory: canceled / shutdown
// paths set skipPersist=true → no runs/<jobID>/<run_id>.json written.
func TestP1_FinishRunSkipPersistDoesNotWriteHistory(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: &fakeRouter{}})
	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.putJobForTest(j)

	runID := mustGenerateRunID()
	s.finishRun(runCtx{jobID: j.ID, runID: runID, startedAt: time.Now(), trigger: TriggerScheduled}, runOutcome{state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: "context canceled", skipPersist: true})

	if _, err := s.Run(jobID, runID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}
	rows := s.ListRuns(jobID, 10, time.Time{})
	if len(rows) != 0 {
		t.Fatalf("expected zero rows for skipPersist run; got %+v", rows)
	}
}

// TestP1_FinishRunSanitisationConsistency: the result/errMsg byte content
// in the persisted CronRun matches what recordResultP0 wrote into
// Job.LastResult / Job.LastError. Otherwise dashboard list view and
// timeline detail would render different bytes for the same run.
func TestP1_FinishRunSanitisationConsistency(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: &fakeRouter{}})
	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.putJobForTest(j)

	// Errors with absolute paths trigger redactPathsInCronError.
	rawErr := "session error: open /etc/secret-config: permission denied"
	runID := mustGenerateRunID()
	s.finishRun(runCtx{jobID: j.ID, runID: runID, startedAt: time.Now(), trigger: TriggerScheduled, snap: jobSnapshot{prompt: "x", workDir: "/wd", fresh: false}}, runOutcome{state: RunStateFailed, errClass: ErrClassSessionError, errMsg: rawErr})

	got, err := s.Run(jobID, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	jobLastErr := s.jobForTest(t, jobID).LastError

	if got.ErrorMsg != jobLastErr {
		t.Errorf("CronRun.ErrorMsg %q diverges from Job.LastError %q", got.ErrorMsg, jobLastErr)
	}
	// Verify path was redacted (regression guard against double-redact).
	if got.ErrorMsg == rawErr {
		t.Errorf("ErrorMsg not redacted: %q", got.ErrorMsg)
	}
}

// TestP1_DeleteJobByIDRemovesRunsSubtree: deleting a job clears its
// runs/<jobID>/ tree synchronously. ~/.claude/projects/... JSONL is not
// touched (we cannot test that here without a real router; doc-level
// guarantee).
func TestP1_DeleteJobByIDRemovesRunsSubtree(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: &fakeRouter{}})
	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.putJobForTest(j)

	// Append one run so the subtree exists.
	s.finishRun(runCtx{jobID: j.ID, runID: mustGenerateRunID(), startedAt: time.Now(), trigger: TriggerScheduled}, runOutcome{state: RunStateSucceeded, result: "x"})
	subtree := filepath.Join(tmp, "runs", jobID)
	if _, err := os.Stat(subtree); err != nil {
		t.Fatalf("subtree should exist after append: %v", err)
	}

	// Delete via the public API.
	if _, err := s.DeleteJobByID(jobID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	if _, err := os.Stat(subtree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("subtree should be gone, got err=%v", err)
	}
}

// TestP1_DisabledStoreNoOps: NewScheduler with empty StorePath has a
// disabled run store — Append paths inside finishRun never panic and
// ListRuns / GetRun return empty.
func TestP1_DisabledStoreNoOps(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})
	if s.runs == nil || s.runs.Enabled() {
		t.Fatal("run store should be disabled when StorePath is empty")
	}
	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.putJobForTest(j)

	// Should not panic.
	s.finishRun(runCtx{jobID: j.ID, runID: mustGenerateRunID(), startedAt: time.Now(), trigger: TriggerScheduled}, runOutcome{state: RunStateSucceeded, result: "x"})
	if rows := s.ListRuns(jobID, 10, time.Time{}); len(rows) != 0 {
		t.Errorf("disabled list: want empty, got %+v", rows)
	}
	if _, err := s.Run(jobID, "abcdef0123456789"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("disabled get: want ErrNotExist, got %v", err)
	}
}
