package cron

import (
	"testing"
)

// TestPauseJobLocked_ReturnsCronRemoveClosure pins R236-QA-03 (#537):
// pauseJobLocked must split its responsibilities into "in-memory mutation
// under lock" (j.Paused=true, j.entryID=0) and "cron.Remove outside lock"
// (returned closure). Without this split, callers that hold s.mu when
// invoking pauseJobLocked block on the unbuffered robfig/cron c.remove
// channel send while the scheduler mutex is still held — exactly the
// lock-order anti-pattern ListAllJobsWithNextRun's godoc warns against.
//
// The contract:
//
//  1. After pauseJobLocked returns successfully on a job with
//     entryID != 0, j.Paused must be true AND j.entryID must be 0.
//  2. The returned cronCleanup closure must be non-nil so callers can
//     defer it without nil-checking.
//  3. Calling cronCleanup releases the cron entry; we don't observe the
//     removal directly (would require introspecting robfig/cron's
//     internal entries) but a follow-up Resume must round-trip
//     cleanly — the entry that gets re-registered must be the
//     successor of the removed one, which only works if cronCleanup
//     actually fired between Pause and Resume.
//  4. On the already-paused error path, the closure is also non-nil
//     (defensive default) so a defer cronCleanup() at the call site
//     stays safe even when err != nil.
func TestPauseJobLocked_ReturnsCronRemoveClosure(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        10,
		AllowNilRouter: true,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@hourly", Prompt: "p", Platform: "x", ChatID: "c", WorkDir: "/tmp"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Active job has entryID != 0 — pauseJobLocked must capture and
	// hand back a non-nil closure.
	s.mu.Lock()
	j := s.jobs[job.ID]
	if j.entryID == 0 {
		s.mu.Unlock()
		t.Fatalf("expected non-zero entryID after AddJob; got 0")
	}
	cleanup, rb, err := s.pauseJobLocked(j)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("pauseJobLocked: unexpected err: %v", err)
	}
	if cleanup == nil {
		t.Fatalf("pauseJobLocked must return a non-nil cleanup closure even on success")
	}
	if rb == nil {
		t.Fatalf("pauseJobLocked must return a non-nil rollback closure even on success (R20260527-COR-1 / #1272)")
	}
	if !j.Paused {
		t.Errorf("j.Paused = false; want true after pauseJobLocked")
	}
	if j.entryID != 0 {
		t.Errorf("j.entryID = %d; want 0 after pauseJobLocked (cron.Remove deferred to cleanup, but j.entryID must be cleared under lock)", j.entryID)
	}

	// cleanup runs OUTSIDE s.mu — this is the whole point of the
	// refactor. It must not panic, must not block.
	cleanup()

	// Already-paused: cleanup must still be non-nil (defensive
	// default) so a defer call at the call site stays safe.
	s.mu.Lock()
	cleanup2, rb2, err2 := s.pauseJobLocked(j)
	s.mu.Unlock()
	if err2 == nil {
		t.Errorf("pauseJobLocked on already-paused job: expected ErrJobAlreadyPaused, got nil")
	}
	if cleanup2 == nil {
		t.Errorf("pauseJobLocked must return non-nil cleanup closure on error path so callers can `defer cleanup()` safely")
	}
	if rb2 == nil {
		t.Errorf("pauseJobLocked must return non-nil rollback closure on error path so callers can defer it safely")
	}
}

// TestPauseJobByID_DoesNotHoldMuDuringCronRemove pins the post-Unlock
// invariant: PauseJobByID drives pauseJobLocked → withJobByID, with the
// cron.Remove closure scheduled into postCleanup so it fires AFTER s.mu
// is released. The test exercises the happy path end-to-end and asserts
// the visible after-state (Paused=true, NextRun=0) so a regression that
// pulls cron.Remove back inside s.mu fails here even before any
// timing-based test catches it.
func TestPauseJobByID_DoesNotHoldMuDuringCronRemove(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        10,
		AllowNilRouter: true,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@hourly", Prompt: "p", Platform: "x", ChatID: "c", WorkDir: "/tmp"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	got, err := s.PauseJobByID(job.ID)
	if err != nil {
		t.Fatalf("PauseJobByID: %v", err)
	}
	if !got.Paused {
		t.Errorf("got.Paused = false; want true")
	}
	// NextRun must be the zero time after pausing (entry is gone from
	// robfig/cron post-cleanup; if the cron.Remove never fired, the
	// entry would still tick and NextRun would still be set).
	if nr := s.NextRun(got); !nr.IsZero() {
		t.Errorf("NextRun after PauseJobByID = %v; want zero (entry removed)", nr)
	}
}
