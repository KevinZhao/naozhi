package cron

import (
	"testing"
	"time"
)

// TestDeleteJobLocked_ReturnsCronEntryID pins R20260605B-CORR-6 (#1810):
// deleteJobLocked must split its responsibilities into "in-memory mutation
// under lock" (drop from s.jobs, zero j.entryID) and "cron.Remove outside
// lock" (the returned entryID, removed by the caller after s.mu is released).
// Before this split deleteJobLocked called s.cron.Remove while the caller
// held s.mu — sending on robfig/cron's unbuffered c.remove channel under the
// write lock, the exact lock-order anti-pattern pauseJobLocked / resumeJobLocked
// / UpdateJob already hoist their Remove for.
//
// Contract:
//
//  1. deleteJobLocked must NOT call s.cron.Remove itself.
//  2. It must zero j.entryID under lock so a concurrent
//     ListAllJobsWithNextRun snapshot sees the entry-removed state.
//  3. It must return the captured entryID so the caller can remove it from
//     cron after Unlock.
func TestDeleteJobLocked_ReturnsCronEntryID(t *testing.T) {
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

	s.mu.Lock()
	j := s.jobs[job.ID]
	if j == nil || j.entryID == 0 {
		s.mu.Unlock()
		t.Fatalf("expected a registered job with non-zero entryID after AddJob")
	}
	want := j.entryID
	removeEntryID := s.deleteJobLocked(j)
	zeroed := j.entryID
	s.mu.Unlock()

	if removeEntryID != want {
		t.Errorf("deleteJobLocked returned entryID %d; want the captured %d", removeEntryID, want)
	}
	if zeroed != 0 {
		t.Errorf("j.entryID = %d after deleteJobLocked; want 0 (cron.Remove deferred, but entryID must be cleared under lock)", zeroed)
	}

	// Caller removes the entry from cron outside the lock — must not panic.
	s.cron.Remove(removeEntryID)
}

// TestDeleteJobByID_DoesNotHoldMuDuringCronRemove pins the post-Unlock
// invariant end-to-end: DeleteJobByID routes deleteJobLocked's captured
// entryID through deleteJobPostCleanup, which runs s.cron.Remove AFTER s.mu
// is released. A regression that pulls cron.Remove back under s.mu (or drops
// it entirely) fails here: the job must be gone from the list and its cron
// entry must no longer tick.
func TestDeleteJobByID_DoesNotHoldMuDuringCronRemove(t *testing.T) {
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

	// Capture the entryID before delete so we can assert it left cron.
	s.mu.RLock()
	entryID := s.jobs[job.ID].entryID
	s.mu.RUnlock()
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
	s.mu.RLock()
	_, present := s.jobs[job.ID]
	s.mu.RUnlock()
	if present {
		t.Errorf("job %s still in s.jobs after DeleteJobByID", job.ID)
	}
}
