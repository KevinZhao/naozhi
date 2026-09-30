package cron

import (
	"testing"
)

// TestPauseJobByID_DoesNotHoldMuDuringCronRemove pins the post-Unlock
// invariant: PauseJobByID drives mutateByID → finishMutation, and the
// cron.Remove runs in finishMutation, AFTER s.tbl.mu is released. The test exercises the happy path end-to-end and asserts
// the visible after-state (Paused=true, NextRun=0) so a regression that
// pulls cron.Remove back inside s.tbl.mu fails here even before any
// timing-based test catches it.
func TestPauseJobByID_DoesNotHoldMuDuringCronRemove(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        10,
		AllowNilRouter: true,
	}, SchedulerDeps{})
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
