package cron

import (
	"testing"
)

// TestUpdateJob_ScheduleChangeRollback_RestoresCachedSched pins R20260602-CR-1:
// when UpdateJob changes the schedule of a non-paused job and registerJob
// fails (invalid new schedule string), the rollback must restore j.cachedSched
// to its pre-update value rather than setting it to nil.
//
// Before the fix the rollback block unconditionally set j.cachedSched = nil,
// discarding the pre-update parsed schedule. HasMissedScheduleCached and
// applyJitterSched both use j.cachedSched for their 1 Hz fanout; a nil
// cachedSched on a still-registered job caused silent fallbacks (period=0,
// missed-schedule always false).
//
// Anchor: R20260602-CR-1.
func TestUpdateJob_ScheduleChangeRollback_RestoresCachedSched(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{MaxJobs: 5, AllowNilRouter: true})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Add a non-paused job with a valid schedule so registerJob populates
	// cachedSched via s.cron.Entry(entryID).Schedule.
	j := &Job{
		Schedule: "@hourly",
		Prompt:   "test",
		Platform: "x",
		ChatID:   "c1",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Capture pre-update cachedSched — must be non-nil for a registered job.
	s.mu.RLock()
	live := s.jobs[j.ID]
	if live == nil {
		s.mu.RUnlock()
		t.Fatal("job missing from s.jobs after AddJob")
	}
	preCachedSched := live.cachedSched
	s.mu.RUnlock()

	if preCachedSched == nil {
		t.Fatal("precondition: cachedSched must be non-nil for a registered active job")
	}

	// Attempt UpdateJob with an invalid schedule string. robfig/cron.AddFunc
	// will reject it, triggering schedRegErr != nil → rollback path.
	invalidSched := "NOT_A_VALID_CRON_SPEC"
	_, err := s.UpdateJob(j.ID, JobUpdate{Schedule: &invalidSched})
	if err == nil {
		t.Fatal("UpdateJob with invalid schedule must return an error (registerJob should fail)")
	}

	// After the rolled-back UpdateJob, cachedSched must be restored to the
	// pre-update value — not nil.
	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.jobs[j.ID]
	if got == nil {
		t.Fatal("job vanished from s.jobs after rolled-back UpdateJob")
	}
	if got.cachedSched == nil {
		t.Fatal("R20260602-CR-1: cachedSched is nil after rollback; pre-update parsed schedule must be restored")
	}
	if got.cachedSched != preCachedSched {
		t.Fatalf("R20260602-CR-1: cachedSched = %v after rollback; want pre-update value %v",
			got.cachedSched, preCachedSched)
	}
	// Schedule string must also be rolled back.
	if got.Schedule != "@hourly" {
		t.Errorf("Schedule = %q after rollback; want %q", got.Schedule, "@hourly")
	}
}
