// scheduler_jobs_list.go: cron Job read-only list / snapshot surface
// (ListJobs, GetJob, ListAllJobsWithNextRun).

package cron

import "time"

// ListJobs returns jobs for a specific chat. Walks the jobsByChat index —
// O(jobs-in-chat), not O(all jobs) — since dashboard polls hit this at 1Hz
// per active chat.
func (s *Scheduler) ListJobs(plat, chatID string) []Job {
	return s.tbl.forChat(chatKeyFor(plat, chatID))
}

// GetJob returns a copy of the job with the given id. The bool is false when
// no such job exists. Read-only; callers that need to mutate go through
// UpdateJob so persistence and cron re-registration stay atomic.
func (s *Scheduler) GetJob(id string) (Job, bool) {
	return s.tbl.snapshot(id)
}

// JobWithNextRun pairs a Job snapshot with its next scheduled run time so
// callers rendering lists (dashboard) don't need a second round-trip per job.
type JobWithNextRun struct {
	Job     Job
	NextRun time.Time
}

// ListAllJobsWithNextRun returns every job plus its next scheduled run.
// Lock strategy: snapshot (Job copy, entryID) under s.tbl.mu.RLock, release, then
// call s.cron.Entries() without holding s.tbl.mu — calling it inside s.tbl.mu would
// invert the lock order the cron dispatcher takes (cron-internal → execute →
// recordResult → s.tbl.mu.Lock). One Entries() snapshot feeds an entryID→Next map,
// O(N) with a single runningMu acquisition, instead of per-job Entry() calls
// that are O(N²) at 1Hz dashboard polling.
func (s *Scheduler) ListAllJobsWithNextRun() []JobWithNextRun {
	// NextRun is patched in by index below once Entries() has been read
	// without the registry lock.
	result, ids := s.tbl.allWithEntryIDs(nil)

	// Single Entries() snapshot, called outside s.tbl.mu (see godoc).
	entries := s.cron.Entries()
	nextByID := make(map[cronEntryID]time.Time, len(entries))
	for _, e := range entries {
		nextByID[e.ID] = e.Next
	}

	for i, id := range ids {
		if id != 0 {
			result[i].NextRun = nextByID[id]
		}
	}
	return result
}
