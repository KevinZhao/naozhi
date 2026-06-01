package cron

import "testing"

// TestUpdateJob_NotifyClear_R249_CR_15 verifies the additive reset-to-nil
// API (#958): setting JobUpdate.NotifyClear to pointer-to-true resets a
// previously-set Job.Notify back to nil (legacy-default / inherit policy),
// while nil or pointer-to-false leaves it untouched. Closes R249-CR-15:
// there was previously no way to reset Notify without editing the store
// file off-line.
func TestUpdateJob_NotifyClear_R249_CR_15(t *testing.T) {
	t.Parallel()

	boolPtr := func(b bool) *bool { return &b }

	t.Run("clear resets a set Notify to nil", func(t *testing.T) {
		t.Parallel()
		s := schedulerForJobsR241GO2Test(t)
		job := &Job{Schedule: "0 * * * *", Prompt: "hi", WorkDir: t.TempDir()}
		if err := s.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}

		// First set an explicit tri-state value.
		set, err := s.UpdateJob(job.ID, JobUpdate{Notify: boolPtr(true)})
		if err != nil {
			t.Fatalf("UpdateJob(set Notify): %v", err)
		}
		if set.Notify == nil || *set.Notify != true {
			t.Fatalf("after set: Notify = %v, want pointer-to-true", set.Notify)
		}

		// Now clear it.
		cleared, err := s.UpdateJob(job.ID, JobUpdate{NotifyClear: boolPtr(true)})
		if err != nil {
			t.Fatalf("UpdateJob(clear): %v", err)
		}
		if cleared.Notify != nil {
			t.Fatalf("after clear: Notify = %v, want nil", *cleared.Notify)
		}
	})

	t.Run("clear=false is a no-op", func(t *testing.T) {
		t.Parallel()
		s := schedulerForJobsR241GO2Test(t)
		job := &Job{Schedule: "0 * * * *", Prompt: "hi", WorkDir: t.TempDir()}
		if err := s.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		if _, err := s.UpdateJob(job.ID, JobUpdate{Notify: boolPtr(false)}); err != nil {
			t.Fatalf("UpdateJob(set Notify=false): %v", err)
		}
		got, err := s.UpdateJob(job.ID, JobUpdate{NotifyClear: boolPtr(false)})
		if err != nil {
			t.Fatalf("UpdateJob(clear=false): %v", err)
		}
		if got.Notify == nil || *got.Notify != false {
			t.Fatalf("clear=false changed Notify: got %v, want pointer-to-false", got.Notify)
		}
	})

	t.Run("clear wins when both Notify and NotifyClear are sent", func(t *testing.T) {
		t.Parallel()
		s := schedulerForJobsR241GO2Test(t)
		job := &Job{Schedule: "0 * * * *", Prompt: "hi", WorkDir: t.TempDir()}
		if err := s.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		got, err := s.UpdateJob(job.ID, JobUpdate{
			Notify:      boolPtr(true),
			NotifyClear: boolPtr(true),
		})
		if err != nil {
			t.Fatalf("UpdateJob(both): %v", err)
		}
		if got.Notify != nil {
			t.Fatalf("clear should win: Notify = %v, want nil", *got.Notify)
		}
	})
}
