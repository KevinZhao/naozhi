package cron

import (
	"testing"
	"time"
)

// TestR249ARCH28_FinishRunNoRunRecordOnJobPersistFailure pins the #992
// two-step-atomicity invariant: cron_jobs.json (the Job's LastSessionID /
// LastResult update) and runs/<jobID>/<runID>.json (the CronRun history
// record) are written by two separate code paths inside finishRun. If the
// Job persist fails (jobPersistOK == false), finishRun MUST NOT also write
// the CronRun history record — otherwise a reader would see a timeline entry
// (CronRun) for a run whose Job-side state was rolled back, i.e. the exact
// disk-divergence the gate at scheduler_finish.go guards against.
//
// recordTerminalResult rolls back the in-memory Job fields on persist
// failure and returns jobPersistOK=false; the `!a.skipPersist && jobPersistOK
// && s.runStore != nil` gate then suppresses the Append. This test drives
// finishRun with a failing marshaler and asserts RecentRuns stays empty.
func TestR249ARCH28_FinishRunNoRunRecordOnJobPersistFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := SchedulerConfig{
		MaxJobs:   5,
		Router:    &fakeRouter{},
		StorePath: dir + "/cron_jobs.json",
	}
	sched := NewScheduler(cfg)
	if sched.runStore == nil || sched.runStore.disabled {
		t.Fatalf("runStore must be enabled for this test (StorePath set)")
	}

	j := &Job{
		ID:         "abcd00000000beef",
		Schedule:   "@every 5m",
		Prompt:     "ping",
		LastResult: "OLD",
	}
	sched.mu.Lock()
	sched.jobs[j.ID] = j
	sched.mu.Unlock()

	// Fail the Job-side persist so recordTerminalResult returns
	// jobPersistOK=false and rolls the in-memory fields back.
	withFailingMarshal(t, sched)

	inflight := sched.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	sched.finishRun(finishArgs{
		job:       j,
		runID:     "00001111deadbeef",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		sessionID: "NEW-SESSION",
		result:    "NEW-RESULT",
		finalizer: finalizer,
	})

	// No CronRun history record may exist: the Append is gated on
	// jobPersistOK, which is false here.
	if runs := sched.RecentRuns(j.ID, 10); len(runs) != 0 {
		t.Fatalf("RecentRuns returned %d record(s) after a failed Job persist; "+
			"want 0 (CronRun Append must be gated on jobPersistOK to avoid "+
			"disk divergence — #992)", len(runs))
	}
}

// TestR249ARCH28_FinishRunWritesRunRecordOnPersistSuccess is the positive
// counterpart: with a working marshaler the Job persist succeeds
// (jobPersistOK=true) and finishRun DOES write the CronRun record. Guards
// against a regression that mistakenly always suppresses the Append (which
// would make the divergence test above pass vacuously).
func TestR249ARCH28_FinishRunWritesRunRecordOnPersistSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := SchedulerConfig{
		MaxJobs:   5,
		Router:    &fakeRouter{},
		StorePath: dir + "/cron_jobs.json",
	}
	sched := NewScheduler(cfg)

	j := &Job{
		ID:       "abcd11111111cafe",
		Schedule: "@every 5m",
		Prompt:   "ping",
	}
	sched.mu.Lock()
	sched.jobs[j.ID] = j
	sched.mu.Unlock()

	inflight := sched.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	sched.finishRun(finishArgs{
		job:       j,
		runID:     "00002222feedface",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		sessionID: "NEW-SESSION",
		result:    "NEW-RESULT",
		finalizer: finalizer,
	})

	if runs := sched.RecentRuns(j.ID, 10); len(runs) != 1 {
		t.Fatalf("RecentRuns returned %d record(s) after a successful persist; want 1", len(runs))
	}
}
