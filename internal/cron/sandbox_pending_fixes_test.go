package cron

// Tests for R20260613-GOLANG-001/002/004 and R20260613-LOGIC-2 fixes.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// ---------------------------------------------------------------------------
// Fix 1 (R20260613-GOLANG-001): reconcileOneSandboxOrphan must not race
// UpdateJob's in-place mutations of *Job fields.
//
// This test exercises the fix by running UpdateJob concurrently with
// reconcileSandboxPending. Under -race it would previously fire on j.Prompt /
// j.WorkDir / j.FreshContext / j.SideEffects reads outside the lock.
// ---------------------------------------------------------------------------

func TestReconcileOrphan_NoRaceWithConcurrentUpdateJob(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	// Write a pending fixture for the existing job.
	writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "aabbccddeeff0022",
		RuntimeSessionID: "run-aabbccddeeff0022-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})

	newPrompt := "updated concurrently"
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Repeatedly mutate the job's fields while reconcile runs.
		for i := 0; i < 20; i++ {
			_, _ = s.UpdateJob(j.ID, JobUpdate{Prompt: &newPrompt})
			time.Sleep(time.Millisecond)
		}
	}()

	// reconcileSandboxPending snapshots j's fields under RLock — no race.
	s.reconcileSandboxPending()
	wg.Wait()

	// The reconcile should still have produced a terminal event.
	waitEnded(t, rec)
}

// ---------------------------------------------------------------------------
// Fix 2 (R20260613-GOLANG-002): RunStateTimedOut must NOT increment
// CronSandboxRunFailedTotal; only RunStateFailed does.
// ---------------------------------------------------------------------------

func TestFinishSandboxRunWith_TimedOutDoesNotIncrementFailedMetric(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	snap := jobSnapshot{
		jobID:  j.ID,
		prompt: j.Prompt,
		label:  jobTitleOrFallback(j),
	}
	a := sandboxExecArgs{
		job:       j,
		snap:      snap,
		runID:     "deadbeef00000001",
		startedAt: time.Now().Add(-5 * time.Minute),
		trigger:   TriggerScheduled,
		finalizer: &runFinalizer{},
		lg:        slog.Default(),
	}

	before := metrics.CronSandboxRunFailedTotal.Value()
	timedOutBefore := metrics.CronSandboxRunTimedOutTotal.Value()

	// TimedOut: must NOT bump CronSandboxRunFailedTotal.
	s.finishSandboxRun(a, RunStateTimedOut, ErrClassSandboxTransport, "", "deadline exceeded", nil)
	waitEnded(t, rec)

	if delta := metrics.CronSandboxRunFailedTotal.Value() - before; delta != 0 {
		t.Fatalf("CronSandboxRunFailedTotal delta = %d for TimedOut, want 0 (double-count bug)", delta)
	}
	// R20260614-LOGIC-9 (#2091): TimedOut MUST bump the dedicated
	// CronSandboxRunTimedOutTotal so failure-only alerts don't miss sandbox
	// deadlines.
	if delta := metrics.CronSandboxRunTimedOutTotal.Value() - timedOutBefore; delta != 1 {
		t.Fatalf("CronSandboxRunTimedOutTotal delta = %d for TimedOut, want 1 (#2091)", delta)
	}
}

func TestFinishSandboxRunWith_FailedIncrementsFailedMetric(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	snap := jobSnapshot{
		jobID:  j.ID,
		prompt: j.Prompt,
		label:  jobTitleOrFallback(j),
	}
	a := sandboxExecArgs{
		job:       j,
		snap:      snap,
		runID:     "deadbeef00000002",
		startedAt: time.Now().Add(-5 * time.Minute),
		trigger:   TriggerScheduled,
		finalizer: &runFinalizer{},
		lg:        slog.Default(),
	}

	before := metrics.CronSandboxRunFailedTotal.Value()
	timedOutBefore := metrics.CronSandboxRunTimedOutTotal.Value()

	s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxFailed, "", "clean failure", nil)
	waitEnded(t, rec)

	if delta := metrics.CronSandboxRunFailedTotal.Value() - before; delta != 1 {
		t.Fatalf("CronSandboxRunFailedTotal delta = %d for Failed, want 1", delta)
	}
	// Failed must NOT bleed into the timeout counter (#2091).
	if delta := metrics.CronSandboxRunTimedOutTotal.Value() - timedOutBefore; delta != 0 {
		t.Fatalf("CronSandboxRunTimedOutTotal delta = %d for Failed, want 0 (#2091)", delta)
	}
}

// ---------------------------------------------------------------------------
// Fix 3 (R20260613-GOLANG-004): deleted-job orphan path must bump
// CronRunEndedTotal + CronRunFailedTotal.
// ---------------------------------------------------------------------------

func TestReconcileOrphan_DeletedJobBumpsMetrics(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	// No job added → s.jobs["nonexistentjobid"] == nil.
	s, rec := sandboxTestScheduler(t, runner, storePath)

	writePendingFixture(t, storePath, sandboxPending{
		JobID: "0123456789abcdef", RunID: "cafebabe00000001",
		RuntimeSessionID: "run-cafebabe00000001-1234567890123456789",
		StartedAtMS:      time.Now().Add(-3 * time.Minute).UnixMilli(),
	})

	endedBefore := metrics.CronRunEndedTotal.Value()
	failedBefore := metrics.CronRunFailedTotal.Value()

	s.reconcileSandboxPending()

	if rec.endedCount() != 0 {
		t.Fatal("no broadcast expected when job is gone")
	}
	if delta := metrics.CronRunEndedTotal.Value() - endedBefore; delta != 1 {
		t.Fatalf("CronRunEndedTotal delta = %d for deleted-job orphan, want 1", delta)
	}
	if delta := metrics.CronRunFailedTotal.Value() - failedBefore; delta != 1 {
		t.Fatalf("CronRunFailedTotal delta = %d for deleted-job orphan, want 1", delta)
	}
}

// TestReconcileOrphan_DeletedJobBumpsStartedTotal pins R20260613-CR-2:
// the nil-job (deleted-while-down) reconcile path must bump
// CronRunStartedTotal in addition to CronRunEndedTotal/CronRunFailedTotal
// so the Started/Ended counters stay balanced. The j!=nil path bumps
// Started via emitRunStarted (scheduler_callbacks.go:100); the nil-job
// path must do the same manually.
func TestReconcileOrphan_DeletedJobBumpsStartedTotal(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	// No job registered → s.jobs[jobID] == nil.
	s, rec := sandboxTestScheduler(t, runner, storePath)

	writePendingFixture(t, storePath, sandboxPending{
		JobID: "0123456789abcdef", RunID: "cafebabe00000002",
		RuntimeSessionID: "run-cafebabe00000002-1234567890123456789",
		StartedAtMS:      time.Now().Add(-4 * time.Minute).UnixMilli(),
	})

	startedBefore := metrics.CronRunStartedTotal.Value()
	endedBefore := metrics.CronRunEndedTotal.Value()

	s.reconcileSandboxPending()

	if rec.endedCount() != 0 {
		t.Fatal("no broadcast expected when job is gone")
	}
	if delta := metrics.CronRunStartedTotal.Value() - startedBefore; delta != 1 {
		t.Fatalf("CronRunStartedTotal delta = %d for deleted-job orphan, want 1 [R20260613-CR-2]", delta)
	}
	if delta := metrics.CronRunEndedTotal.Value() - endedBefore; delta != 1 {
		t.Fatalf("CronRunEndedTotal delta = %d for deleted-job orphan, want 1 [R20260613-CR-2]", delta)
	}
}

// ---------------------------------------------------------------------------
// Fix 4 (R20260613-LOGIC-2): stopSandboxRunsForJob must skip records whose
// RunID fails IsValidID (log-injection guard).
// ---------------------------------------------------------------------------

func TestStopSandboxRunsForJob_InvalidRunIDSkipped(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	jobID := "0123456789abcdef"

	// Write a pending record with a tampered RunID containing a newline.
	pdir := filepath.Join(filepath.Dir(storePath), "sandboxpending")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := sandboxPending{
		JobID:            jobID,
		RunID:            "evil\ninjected", // fails IsValidID
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	}
	raw, _ := json.Marshal(p)
	path := filepath.Join(pdir, "evil.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s.stopSandboxRunsForJob(jobID)

	// The invalid record must have been skipped — StopSession not called.
	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 0 {
		t.Fatalf("StopSession called %d time(s) for invalid RunID record; want 0", nStopped)
	}
	// File kept (not removed) — the guard skips, does not delete.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file with invalid RunID should be kept (skipped), not removed")
	}
}

func TestStopSandboxRunsForJob_ValidRunIDProcessed(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	jobID := "0123456789abcdef"

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: jobID, RunID: "feedfacefeedface",
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})

	s.stopSandboxRunsForJob(jobID)

	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 1 {
		t.Fatalf("StopSession called %d time(s) for valid RunID; want 1", nStopped)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file must be removed after confirmed stop with valid RunID")
	}
}
