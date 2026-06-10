package cron

import (
	"sync/atomic"
	"testing"
	"time"
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
	s.mu.Lock()
	s.jobs[jobID].Paused = true
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow */)

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
	if got.JobID != jobID {
		t.Errorf("job_id: want %q, got %q", jobID, got.JobID)
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

	// Cross-lock race: caller resolved cur from s.jobs, released RLock,
	// and a Delete landed before CAS. Emulate by removing the s.jobs
	// entry directly.
	s.mu.Lock()
	delete(s.jobs, j.ID)
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow */)

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

// TestExecuteOpt_PostCASPausedRecheck_TriggerNow exercises the
// R20260527122801-CR-8 (#1322) fix: a Pause that lands AFTER the dispatch
// helper releases s.mu but BEFORE executeOpt's CAS gate must abort the
// run. Without the post-CAS recheck the TriggerNow path would reach the
// router.GetOrCreate call and burn a real run on a paused job.
//
// Strategy: register a job, then mark it Paused under s.mu (simulating
// the post-RLock-release / pre-CAS race window) before calling executeOpt
// directly with viaTriggerNow=true. The router stub increments a counter
// on GetOrCreate; the test asserts the counter stays at zero.
func TestExecuteOpt_PostCASPausedRecheck_TriggerNow(t *testing.T) {
	t.Parallel()

	sr := &jitterStubRouter{}
	s := NewScheduler(SchedulerConfig{
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   10,
	}, SchedulerDeps{
		Router: sr,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{
		ID:       "paused-via-race",
		Schedule: "@every 1h",
		Prompt:   "should-not-run",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Simulate the cross-lock race: by the time executeOpt runs, the job
	// has been paused. Mutate Paused directly under s.mu so we don't
	// require a full PauseJobByID dance (which would also tear down the
	// cron entry and is orthogonal to this test).
	s.mu.Lock()
	s.jobs[j.ID].Paused = true
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.executeOpt(j, true /* viaTriggerNow */)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("executeOpt blocked >2s; post-CAS paused recheck did not abort early")
	}

	if got := atomic.LoadInt64(&sr.calls); got != 0 {
		t.Fatalf("router.GetOrCreate called %d times; expected 0 — paused recheck failed", got)
	}
}

// TestExecuteOpt_PostCASDeletedRecheck_TriggerNow mirrors the paused test
// for the deleted-job side of the race window — a DeleteJob that lands
// between dispatch and CAS must also abort the TriggerNow run before any
// router work.
func TestExecuteOpt_PostCASDeletedRecheck_TriggerNow(t *testing.T) {
	t.Parallel()

	sr := &jitterStubRouter{}
	s := NewScheduler(SchedulerConfig{
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   10,
	}, SchedulerDeps{
		Router: sr,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{
		ID:       "deleted-via-race",
		Schedule: "@every 1h",
		Prompt:   "should-not-run",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Simulate the dispatch-vs-delete race: caller resolved cur from
	// s.jobs, released RLock, and a Delete landed before CAS. We can't
	// call DeleteJobByID because it also runs router.Reset / postCleanup
	// in ways that aren't on the hot path of this test; mutating the map
	// directly is the smallest reproduction of the executeOpt-visible
	// state.
	s.mu.Lock()
	delete(s.jobs, j.ID)
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.executeOpt(j, true /* viaTriggerNow */)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("executeOpt blocked >2s; post-CAS deleted recheck did not abort early")
	}

	if got := atomic.LoadInt64(&sr.calls); got != 0 {
		t.Fatalf("router.GetOrCreate called %d times; expected 0 — deleted recheck failed", got)
	}
}
