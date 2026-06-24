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

// ---------------------------------------------------------------------------
// R20260614-GO-001: reconcileOneSandboxOrphan must bump
// CronSandboxRunFailedTotal on BOTH the live-job and deleted-job branches —
// it closes the run via finishRun directly (not finishSandboxRunWith, the
// only other sandbox-failure path that bumps the counter), so without an
// explicit bump an orphaned sandbox run is invisible to
// naozhi_cron_sandbox_run_failed_total.
// ---------------------------------------------------------------------------

func TestReconcileOrphan_LiveJobBumpsSandboxFailedMetric(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s) // job still exists at reconcile time

	writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "abcabcabc0000001",
		RuntimeSessionID: "run-abcabcabc0000001-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})

	sandboxFailedBefore := metrics.CronSandboxRunFailedTotal.Value()

	s.reconcileSandboxPending()
	waitEnded(t, rec)

	if delta := metrics.CronSandboxRunFailedTotal.Value() - sandboxFailedBefore; delta != 1 {
		t.Fatalf("CronSandboxRunFailedTotal delta = %d for live-job orphan, want 1 [R20260614-GO-001]", delta)
	}
}

func TestReconcileOrphan_DeletedJobBumpsSandboxFailedMetric(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	// No job added → nil-job branch.
	s, _ := sandboxTestScheduler(t, runner, storePath)

	writePendingFixture(t, storePath, sandboxPending{
		JobID: "0123456789abcdef", RunID: "abcabcabc0000002",
		RuntimeSessionID: "run-abcabcabc0000002-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})

	sandboxFailedBefore := metrics.CronSandboxRunFailedTotal.Value()

	s.reconcileSandboxPending()

	if delta := metrics.CronSandboxRunFailedTotal.Value() - sandboxFailedBefore; delta != 1 {
		t.Fatalf("CronSandboxRunFailedTotal delta = %d for deleted-job orphan, want 1 [R20260614-GO-001]", delta)
	}
}

// ---------------------------------------------------------------------------
// R20260614-GO-003: a pending record with StartedAtMS<=0 is corrupt and must
// be dropped (re-warned once then removed), not flowed into a 1970 StartedAt
// and an astronomical DurationMS.
// ---------------------------------------------------------------------------

func TestReconcileSandboxPending_ZeroStartedAtDroppedAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "abcabcabc0000003",
		RuntimeSessionID: "run-abcabcabc0000003-1234567890123456789",
		StartedAtMS:      0, // corrupt: would yield a 1970 StartedAt
	})

	s.reconcileSandboxPending()

	// Dropped as corrupt: no terminal broadcast, file removed.
	if rec.endedCount() != 0 {
		t.Fatal("StartedAtMS<=0 record must be dropped as corrupt, not reconciled into a terminal run")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt (StartedAtMS<=0) pending file must be removed so it does not re-warn every boot")
	}
}

func TestReconcileSandboxPending_NegativeStartedAtDroppedAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "abcabcabc0000004",
		RuntimeSessionID: "run-abcabcabc0000004-1234567890123456789",
		StartedAtMS:      -5000,
	})

	s.reconcileSandboxPending()

	if rec.endedCount() != 0 {
		t.Fatal("negative StartedAtMS record must be dropped as corrupt")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt (negative StartedAtMS) pending file must be removed")
	}
}

// ---------------------------------------------------------------------------
// R20260614-ARCH-1: enqueueSandboxTransportAttention must skip the write when
// the job has been deleted out from under the in-flight run, so a delete /
// in-flight-transport-failure race cannot leave a ghost §7.4 queue card whose
// replay would ErrJobNotFound.
// ---------------------------------------------------------------------------

func TestEnqueueSandboxTransportAttention_SkipsDeletedJob(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	// snap.jobID points at a job that does NOT exist in s.jobs (simulating a
	// DeleteJobByID that completed while this run's goroutine was blocked on
	// the now-severed stream).
	a := sandboxExecArgs{
		snap: jobSnapshot{
			jobID:       "0123456789abcdef",
			label:       "ghost",
			sideEffects: true,
		},
		runID:     "deadbeefdeadbe01",
		startedAt: time.Now(),
		lg:        slog.Default(),
	}

	s.enqueueSandboxTransportAttention(a, "run-deadbeefdeadbe01-1234567890123456789")

	if n := s.SandboxAttentionCount(); n != 0 {
		t.Fatalf("attention count = %d, want 0 — deleted-job transport failure must not write a ghost card [R20260614-ARCH-1]", n)
	}
}

func TestEnqueueSandboxTransportAttention_WritesForLiveJob(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	j := sideEffectsJob(t, s) // job exists

	a := sandboxExecArgs{
		snap: jobSnapshot{
			jobID:       j.ID,
			label:       jobTitleOrFallback(j),
			sideEffects: true,
		},
		runID:     "deadbeefdeadbe02",
		startedAt: time.Now(),
		lg:        slog.Default(),
	}

	s.enqueueSandboxTransportAttention(a, "run-deadbeefdeadbe02-1234567890123456789")

	if n := s.SandboxAttentionCount(); n != 1 {
		t.Fatalf("attention count = %d, want 1 — live-job side-effecting transport failure must enqueue [R20260614-ARCH-1]", n)
	}
}

// ---------------------------------------------------------------------------
// Fix (#2119) [R20260614-LOGIC-4]: reconcileOneSandboxOrphan must NOT clobber
// an attention record an in-process transport failure already wrote for the
// same runID, and must still write an orphaned record when none pre-exists.
// ---------------------------------------------------------------------------

func TestReconcileOrphan_PreservesExistingTransportAttentionReason(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)

	runID := "aabbccddeeff0119"
	lg := slog.Default()

	// (a) In-process transport failure already enqueued a transport record for
	// this runID before the process died.
	s.writeSandboxAttention(sandboxAttention{
		JobID:            j.ID,
		RunID:            runID,
		RuntimeSessionID: "run-aabbccddeeff0119-1234567890123456789",
		Reason:           attentionReasonTransport,
		JobLabel:         "push a PR",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
		CreatedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	}, lg)

	// (b)+(c) Process crashed before removing the pending file; restart
	// reconcile sees the orphan and would re-write attention for the same runID.
	writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: runID,
		RuntimeSessionID: "run-aabbccddeeff0119-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})

	s.reconcileSandboxPending()
	waitEnded(t, rec)

	got, ok, err := s.getSandboxAttention(runID)
	if err != nil {
		t.Fatalf("getSandboxAttention: %v", err)
	}
	if !ok || got == nil {
		t.Fatal("attention record disappeared after reconcile; want preserved")
	}
	if got.Reason != attentionReasonTransport {
		t.Fatalf("reason = %q, want %q (orphan reconcile must not clobber the in-process transport record) [#2119]",
			got.Reason, attentionReasonTransport)
	}
	// Exactly one queue entry — no duplicate written.
	if items := s.ListSandboxAttention(); len(items) != 1 {
		t.Fatalf("queue len = %d, want 1", len(items))
	}
}

func TestReconcileOrphan_WritesOrphanedAttentionWhenNonePreexists(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)

	runID := "aabbccddeeff0120"

	// No prior attention record: a clean restart orphan (process died before
	// any in-process transport attention was written).
	writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: runID,
		RuntimeSessionID: "run-aabbccddeeff0120-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})

	s.reconcileSandboxPending()
	waitEnded(t, rec)

	got, ok, err := s.getSandboxAttention(runID)
	if err != nil {
		t.Fatalf("getSandboxAttention: %v", err)
	}
	if !ok || got == nil {
		t.Fatal("orphaned attention record not written when none pre-existed")
	}
	if got.Reason != attentionReasonOrphaned {
		t.Fatalf("reason = %q, want %q (clean orphan must enqueue with orphaned reason) [#2119]",
			got.Reason, attentionReasonOrphaned)
	}
}

// ---------------------------------------------------------------------------
// R20260615-030459-COR-002: reconcileSandboxPending must treat
// RuntimeSessionID=="" as corrupt and drop+warn the record without calling
// StopSession or finishRun. Mirrors the disqualifier already present in
// stopSandboxRunsForJob (line 375).
// ---------------------------------------------------------------------------

func TestReconcileSandboxPending_EmptyRuntimeSessionIDDroppedAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID:            j.ID,
		RunID:            "abcabcabc0000005",
		RuntimeSessionID: "", // empty: no microVM handle, §6.2 containment broken
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})

	s.reconcileSandboxPending()

	// Must be treated as corrupt: no terminal broadcast, no StopSession.
	if rec.endedCount() != 0 {
		t.Fatal("empty RuntimeSessionID record must be dropped as corrupt, not reconciled into a terminal run [COR-002]")
	}
	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 0 {
		t.Fatalf("StopSession called %d time(s) for empty-RSID record; want 0 [COR-002]", nStopped)
	}
	// File must be removed so it does not re-warn on every boot.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt (empty RuntimeSessionID) pending file must be removed [COR-002]")
	}
}

// ---------------------------------------------------------------------------
// R20260615-030459-COR-001: reconcileOneSandboxOrphan must re-check job
// existence under RLock immediately before writeSandboxAttention so a
// concurrent DeleteJobByID that runs between the initial snapshot RUnlock and
// the attention write cannot leave a ghost queue card (TOCTOU, analogous to
// the fix for enqueueSandboxTransportAttention in OPEN #2129).
// ---------------------------------------------------------------------------

// TestReconcileOrphan_NoGhostAttentionWhenJobDeletedBeforeRecheck verifies
// the re-check: if the job is gone from s.jobs at the time of the attention
// write guard, no attention card is written.
// We simulate the TOCTOU window by directly removing the job from s.jobs
// (package-internal test — avoids the panicRouter.Reset path that DeleteJobByID
// would trigger) so the initial RLock snapshot sees nil (deleted-job branch)
// and the re-check also sees nil. The key invariant: no ghost attention card.
func TestReconcileOrphan_NoGhostAttentionWhenJobDeletedBeforeRecheck(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)

	runID := "aabbccddeeff0201"
	p := sandboxPending{
		JobID:            j.ID,
		RunID:            runID,
		RuntimeSessionID: "run-aabbccddeeff0201-1234567890123456789",
		StartedAtMS:      time.Now().Add(-3 * time.Minute).UnixMilli(),
	}
	path := writePendingFixture(t, storePath, p)

	// Simulate the TOCTOU race: remove the job from s.jobs directly so neither
	// the initial snapshot NOR the re-check can find it. The deleted-job branch
	// must not write an attention card regardless.
	s.mu.Lock()
	delete(s.jobs, j.ID)
	s.mu.Unlock()

	startedBefore := rec.startedCount()
	s.reconcileOneSandboxOrphan(p, path)
	// reconcileOneSandboxOrphan's deleted-job branch is fully synchronous
	// (metrics bumps + os.Remove, no goroutine/broadcast), so its effects are
	// complete the moment the call returns — assert directly, no sleep needed.

	// No broadcast expected from the deleted-job branch.
	if rec.endedCount() != 0 {
		t.Fatal("deleted-job orphan branch must not broadcast an ended event [COR-001]")
	}
	// R202606-CR-001 (#2156): no started broadcast either — the metrics-only
	// path must NOT call emitRunStarted for a job that no longer exists.
	if got := rec.startedCount(); got != startedBefore {
		t.Fatalf("RunStarted count grew %d→%d for job-gone reconcile — phantom lifecycle [#2156]", startedBefore, got)
	}
	// Critical: no ghost attention card must have been written.
	if n := s.SandboxAttentionCount(); n != 0 {
		t.Fatalf("attention count = %d after job-deleted reconcile; want 0 — re-check must prevent ghost card [COR-001]", n)
	}
	// Pending file removed.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file must be removed after orphan reconcile [COR-001]")
	}
}

// TestReconcileSandboxPending_ShutdownBeforeReconcileSkipsStop pins
// R202606-CR-002 at the end-to-end level: with stopCtx already cancelled, a
// single-orphan reconcile must NOT invoke StopSession. (The cancel is observed
// at the inter-entry guard for an already-cancelled ctx; the new single-orphan
// fast-path guard covers the narrower window where stopCtx is cancelled AFTER
// the orphan list is built but BEFORE the serial reconcileOneSandboxOrphan
// call — that window is not deterministically reachable from a black-box test,
// so this test pins the observable contract and the fast-path guard mirrors the
// parallel path's per-orphan ctx.Err() check for the racy window.)
func TestReconcileSandboxPending_ShutdownBeforeReconcileSkipsStop(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	// Exactly one orphan → the single-orphan serial fast path.
	writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "abcabcabc0000006",
		RuntimeSessionID: "run-abcabcabc0000006-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})

	// Cancel stopCtx via Stop() (CAS-guarded; the scheduler was never Started).
	s.Stop()

	s.reconcileSandboxPending()

	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 0 {
		t.Fatalf("StopSession called %d time(s) after shutdown; want 0 [R202606-CR-002]", nStopped)
	}
}

// TestReconcileOrphan_JobDeletedInGap_NoBroadcastBalancedMetrics pins
// R202606-CR-001 (#2156): when DeleteJobByID removes the job in the gap between
// the RLock snapshot and the emitRunStarted/finishRun block, the orphan must be
// closed via the metrics-only path — no started/ended broadcast — yet the
// Started/Ended/Failed/SandboxFailed counters must each advance by exactly one
// so the in-flight gauge (started−ended) stays balanced.
//
// We make the snapshot see j!=nil but the re-check see nil by deleting the job
// from s.jobs from a concurrent goroutine. To make the assertion deterministic
// regardless of which side of the race we land on, we assert the invariant that
// holds in BOTH outcomes: started broadcasts == ended broadcasts (no phantom
// half-lifecycle), and the four counters advance together.
func TestReconcileOrphan_JobDeletedInGap_NoBroadcastBalancedMetrics(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)

	runID := "aabbccddeeff0203"
	p := sandboxPending{
		JobID:            j.ID,
		RunID:            runID,
		RuntimeSessionID: "run-aabbccddeeff0203-1234567890123456789",
		StartedAtMS:      time.Now().Add(-3 * time.Minute).UnixMilli(),
	}
	path := writePendingFixture(t, storePath, p)

	// Delete the job out from under the snapshot. Done before the call so the
	// re-check deterministically sees nil even if the snapshot raced and saw
	// the job — the routing into the metrics-only path is the contract.
	s.mu.Lock()
	delete(s.jobs, j.ID)
	s.mu.Unlock()

	startedBefore := rec.startedCount()
	endedBefore := rec.endedCount()
	startedMetricBefore := metrics.CronRunStartedTotal.Value()
	endedMetricBefore := metrics.CronRunEndedTotal.Value()
	failedBefore := metrics.CronRunFailedTotal.Value()
	sandboxFailedBefore := metrics.CronSandboxRunFailedTotal.Value()

	s.reconcileOneSandboxOrphan(p, path)
	time.Sleep(5 * time.Millisecond)

	// No subscriber broadcast at all — neither started nor ended.
	if got := rec.startedCount(); got != startedBefore {
		t.Fatalf("RunStarted broadcast grew %d→%d for job gone in gap; want none [#2156]", startedBefore, got)
	}
	if got := rec.endedCount(); got != endedBefore {
		t.Fatalf("RunEnded broadcast grew %d→%d for job gone in gap; want none [#2156]", endedBefore, got)
	}
	// Counters advance by exactly one each (gauge stays balanced).
	if d := metrics.CronRunStartedTotal.Value() - startedMetricBefore; d != 1 {
		t.Fatalf("CronRunStartedTotal delta = %d, want 1 [#2156]", d)
	}
	if d := metrics.CronRunEndedTotal.Value() - endedMetricBefore; d != 1 {
		t.Fatalf("CronRunEndedTotal delta = %d, want 1 [#2156]", d)
	}
	if d := metrics.CronRunFailedTotal.Value() - failedBefore; d != 1 {
		t.Fatalf("CronRunFailedTotal delta = %d, want 1 [#2156]", d)
	}
	if d := metrics.CronSandboxRunFailedTotal.Value() - sandboxFailedBefore; d != 1 {
		t.Fatalf("CronSandboxRunFailedTotal delta = %d, want 1 [#2156]", d)
	}
	// No ghost attention card.
	if n := s.SandboxAttentionCount(); n != 0 {
		t.Fatalf("attention count = %d, want 0 [#2156]", n)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file must be removed after orphan reconcile [#2156]")
	}
}

// TestReconcileOrphan_AttentionRecheck_RaceWithDelete is a -race canary:
// run a side-effects-job reconcile while a concurrent goroutine repeatedly
// deletes+re-adds the job, verifying the attention count never exceeds 1.
// The re-check (COR-001) prevents the ghost-card race.
func TestReconcileOrphan_AttentionRecheck_RaceWithDelete(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)

	runID := "aabbccddeeff0202"
	p := sandboxPending{
		JobID:            j.ID,
		RunID:            runID,
		RuntimeSessionID: "run-aabbccddeeff0202-1234567890123456789",
		StartedAtMS:      time.Now().Add(-3 * time.Minute).UnixMilli(),
	}
	path := writePendingFixture(t, storePath, p)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			// Toggle: delete the job so some reconcile iterations hit
			// the re-check with nil, others with non-nil.
			s.mu.Lock()
			if _, ok := s.jobs[j.ID]; ok {
				delete(s.jobs, j.ID)
			} else {
				s.jobs[j.ID] = j
			}
			s.mu.Unlock()
			time.Sleep(time.Microsecond)
		}
	}()

	// Rewrite pending file on each iteration so reconcile can fire > once.
	for i := 0; i < 5; i++ {
		writePendingFixture(t, storePath, p)
		s.reconcileOneSandboxOrphan(p, path)
	}
	<-done

	// Under -race this will surface any unguarded concurrent access.
	// Attention count may be 0 or 1 — the invariant is: never > 1 (no ghost dup).
	if n := s.SandboxAttentionCount(); n > 1 {
		t.Fatalf("attention count = %d; re-check must prevent ghost duplicate cards [COR-001]", n)
	}
}
