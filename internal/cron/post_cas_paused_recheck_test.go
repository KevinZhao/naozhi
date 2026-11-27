package cron

import (
	"sync/atomic"
	"testing"
)

// TestExecuteOpt_PostCASPausedRecheck_EmitsSyntheticSkipped pins
// R040034-CR-1 (#1410): the post-CAS paused recheck used to silently
// drop the run with only a Debug log. Subscribers (dashboard
// "running" counter / run-list timeline) saw nothing for the 1-2µs
// cross-lock window where Pause races dispatch — split with the
// router-missing precedent (#1323) which already emits a synthetic
// pair. Now both halves of the recheck (pause + delete) emit
// started→ended pairs with distinguishing error_class so the wire
// stays consistent.
func TestExecuteOpt_PostCASPausedRecheck_EmitsSyntheticSkipped(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	sr := &jitterStubRouter{}
	s := NewScheduler(SchedulerConfig{
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   10,
	}, SchedulerDeps{
		Router:    sr,
		Telemetry: rec,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{
		Schedule: "@every 1h",
		Prompt:   "should-not-run",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	jobID := j.ID

	// Cross-lock race: by the time executeOpt runs, the job is paused.
	s.editJobForTest(t, jobID, func(j *Job) { j.Paused = true })

	s.executeOpt(j.ID, true /* viaTriggerNow */)

	if got := rec.endedCount(); got != 1 {
		t.Fatalf("want 1 ended event from paused-recheck synthetic, got %d", got)
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateSkipped {
		t.Errorf("state: want skipped, got %q", got.State)
	}
	if got.ErrorClass != ErrClassPausedConcurrent {
		t.Errorf("error_class: want paused_concurrent, got %q", got.ErrorClass)
	}
	if got.OwnerID != jobID {
		t.Errorf("job_id: want %q, got %q", jobID, got.OwnerID)
	}
	if got.Trigger != TriggerManual {
		t.Errorf("trigger: want manual, got %q", got.Trigger)
	}
	if got := atomic.LoadInt64(&sr.calls); got != 0 {
		t.Errorf("router.GetOrCreate calls: want 0, got %d", got)
	}
}

// TestExecuteOpt_PostCASDeletedRecheck_EmitsSyntheticSkipped is the
// delete-side counterpart to the paused-recheck emit test (#1410).
func TestExecuteOpt_PostCASDeletedRecheck_EmitsSyntheticSkipped(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	sr := &jitterStubRouter{}
	s := NewScheduler(SchedulerConfig{
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   10,
	}, SchedulerDeps{
		Router:    sr,
		Telemetry: rec,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{
		ID:       "deleted-emit",
		Schedule: "@every 1h",
		Prompt:   "should-not-run",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Cross-lock race: caller resolved cur from s.tbl.jobs, released RLock,
	// and a Delete landed before CAS. Emulate by removing the s.tbl.jobs
	// entry directly.
	s.dropJobForTest(j.ID)

	s.executeOpt(j.ID, true /* viaTriggerNow */)

	if got := rec.endedCount(); got != 1 {
		t.Fatalf("want 1 ended event from deleted-recheck synthetic, got %d", got)
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateSkipped {
		t.Errorf("state: want skipped, got %q", got.State)
	}
	if got.ErrorClass != ErrClassDeletedConcurrent {
		t.Errorf("error_class: want deleted_concurrent, got %q", got.ErrorClass)
	}
	if got := atomic.LoadInt64(&sr.calls); got != 0 {
		t.Errorf("router.GetOrCreate calls: want 0, got %d", got)
	}
}
