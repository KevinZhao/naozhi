package cron

import (
	"testing"
	"time"
)

// TestDeleteJobByID_DoesNotHoldMuDuringCronRemove pins the post-Unlock
// invariant end-to-end: DeleteJobByID routes deleteLocked's captured
// entryID through deleteJobPostCleanup, which runs s.cron.Remove AFTER s.tbl.mu
// is released. A regression that pulls cron.Remove back under s.tbl.mu (or drops
// it entirely) fails here: the job must be gone from the list and its cron
// entry must no longer tick.
func TestDeleteJobByID_DoesNotHoldMuDuringCronRemove(t *testing.T) {
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

	// Capture the entryID before delete so we can assert it left cron.
	entryID := s.jobForTest(t, job.ID).entryID
	if entryID == 0 {
		t.Fatalf("expected non-zero entryID after AddJob")
	}

	got, err := s.DeleteJobByID(job.ID)
	if err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	if got == nil || got.ID != job.ID {
		t.Fatalf("DeleteJobByID returned %+v; want the deleted job", got)
	}

	// The cron entry must be removed. robfig/cron's Remove on a running
	// scheduler is processed asynchronously by the run() goroutine (it sends
	// on the unbuffered c.remove channel), so poll briefly rather than
	// reading Entries() synchronously. If cron.Remove never fired the entry
	// would tick forever and this loop would time out.
	removed := false
	for i := 0; i < 200; i++ {
		present := false
		for _, e := range s.cron.Entries() {
			if e.ID == entryID {
				present = true
				break
			}
		}
		if !present {
			removed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !removed {
		t.Fatalf("cron entry %d still present after DeleteJobByID; cron.Remove never fired", entryID)
	}
	// And the job must be gone from the scheduler.
	s.tblForTest().mu.RLock()
	_, present := s.tblForTest().jobs[job.ID]
	s.tblForTest().mu.RUnlock()
	if present {
		t.Errorf("job %s still in s.tbl.jobs after DeleteJobByID", job.ID)
	}
}
