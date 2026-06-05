package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestFinishRunRunStoreAppendFail pins the TOLERATED half of the R249-ARCH-28
// (#992) over-report-only persistence invariant — the direction
// TestPersistOrdering_RunsNeverDivergeAheadOfJob does NOT cover.
//
// finishRun's two-step terminal write is non-transactional:
//  1. recordTerminalResult writes the Job fields (LastResult / LastErrorClass /
//     RunCounters) to cron_jobs.json and returns jobPersistOK.
//  2. ONLY when jobPersistOK==true does runStore.Append write
//     runs/<jobID>/<runID>.json.
//
// The forbidden direction (runs/ record without a Job-side counter) is gated
// out by jobPersistOK and pinned by the divergence test. This test pins the
// SAFE direction: Job-side persist SUCCEEDS but runStore.Append FAILS. The
// design tolerates this as "over-report" — cron_jobs.json shows the run, the
// per-state metric bumped, but runs/<jobID>/ lacks the timeline record. It is
// observable (writeFailedOtherTotal bumps) and self-heals on the next run; it
// is NOT under-report (Job side is the authoritative list source).
//
// Append failure (not marshal failure) is injected by chmod 0o500 on the
// per-job runs dir AFTER seeding jobDirEnsured, mirroring
// runstore_write_failed_counter_test.go so jobPersistOK stays true while
// WriteFileAtomic's rename inside the dir fails with EACCES.
//
// Skip-on-root: a root euid bypasses the 0o500 perm gate so the Append would
// succeed, defeating the premise (same rationale as the write-failed counter
// pin). CI runs non-root so the gate fires.
func TestFinishRunRunStoreAppendFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires non-root euid; perm gate is bypassed otherwise")
	}

	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron.json")
	s := NewScheduler(SchedulerConfig{
		StorePath: storePath,
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	if s.runStore == nil || s.runStore.disabled {
		t.Fatal("runStore must be enabled for this test (StorePath set)")
	}

	j := &Job{
		Schedule: "@every 1h",
		Prompt:   "ping",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true, // avoid registering a live cron entry
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Make the per-job runs dir read-only so runStore.Append's WriteFileAtomic
	// rename fails (POSIX: rename needs write+execute on the containing dir),
	// while leaving the Job-side persist (cron_jobs.json) untouched. Seed
	// jobDirEnsured first so Append takes the hot path and errors on the write,
	// not on an ensureJobDir MkdirAll. Harness mirrors
	// runstore_write_failed_counter_test.go:38-49.
	jobDir := filepath.Join(s.runStore.root, j.ID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatalf("mkdir job runs dir: %v", err)
	}
	s.runStore.jobDirEnsured.Store(j.ID, struct{}{})
	if err := os.Chmod(jobDir, 0o500); err != nil {
		t.Fatalf("chmod job runs dir: %v", err)
	}
	defer os.Chmod(jobDir, 0o700) //nolint:errcheck // best-effort so t.TempDir RemoveAll works.

	// Baselines: the package-global metric counter and the runStore's
	// write-failed totals. Assert deltas, not absolutes — other tests in the
	// package may have bumped the global expvar.
	succ0 := metrics.CronRunSucceededTotal.Value()
	df0, ot0 := s.runStore.WriteFailedTotals()

	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	// Drive the full terminal saga with a successful Job persist. Test
	// completing without panic is itself part of the assertion (finishRun must
	// not panic when Append fails after the Job side landed).
	s.finishRun(finishArgs{
		job:       j,
		runID:     "0123456789abcdef", // valid 16-hex so Append reaches the write
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		sessionID: "sess-1",
		result:    "ok",
		finalizer: finalizer,
	})

	// (a) Job side landed in-memory: recordTerminalResult mutated j directly.
	if j.LastResult != "ok" {
		t.Errorf("Job.LastResult: want %q, got %q", "ok", j.LastResult)
	}
	if got := j.RunCounters.Succeeded; got != 1 {
		t.Errorf("Job.RunCounters.Succeeded: want 1, got %d", got)
	}

	// (a') Job side landed on disk: cron_jobs.json reflects the success. This
	// is the authoritative list source the over-report tolerance leans on.
	loaded, err := loadJobs(storePath)
	if err != nil {
		t.Fatalf("reload cron_jobs.json: %v", err)
	}
	dj, ok := loaded[j.ID]
	if !ok {
		t.Fatalf("reloaded store missing job %s", j.ID)
	}
	if dj.LastResult != "ok" {
		t.Errorf("persisted Job.LastResult: want %q, got %q", "ok", dj.LastResult)
	}
	if dj.RunCounters.Succeeded != 1 {
		t.Errorf("persisted Job.RunCounters.Succeeded: want 1, got %d", dj.RunCounters.Succeeded)
	}

	// (b) Metric bumped: the per-state counter fires because jobPersistOK==true,
	// independent of the Append outcome (bump precedes append in finishRun).
	if delta := metrics.CronRunSucceededTotal.Value() - succ0; delta != 1 {
		t.Errorf("CronRunSucceededTotal delta: want 1, got %d", delta)
	}

	// (c) Append failure is observable: the "other" (non-ENOSPC, here EACCES)
	// write-failed counter bumps by exactly 1; diskFull stays put.
	df1, ot1 := s.runStore.WriteFailedTotals()
	if ot1 != ot0+1 {
		t.Errorf("writeFailedOtherTotal: want +1, got delta %d", ot1-ot0)
	}
	if df1 != df0 {
		t.Errorf("writeFailedDiskFullTotal must not bump on EACCES: got delta %d", df1-df0)
	}

	// (e) The dropped over-report record left no run file: runs/<jobID>/ has no
	// readable run record (the write failed). The dir exists (we created it)
	// but contains no successfully-written run JSON.
	entries, derr := os.ReadDir(jobDir)
	if derr != nil {
		// Reading a 0o500 dir is permitted (read+execute), so this should
		// succeed; if it fails the premise is unmet.
		t.Fatalf("read job runs dir: %v", derr)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			t.Errorf("expected no run record on Append failure, found %q", e.Name())
		}
	}
}
