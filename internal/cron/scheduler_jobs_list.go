// scheduler_jobs_list.go: cron Job read-only list / snapshot surface
// (PerChatJobCount, ListJobs, GetJob, ListJobsWithNextRun,
// ListAllJobsWithNextRun) plus the sync.Pool scratch containers only this
// cluster uses.

package cron

import (
	"sync"
	"time"
)

// listEntryIDsPool reuses the transient []cronEntryID scratch slice the
// two-phase list snapshots record under RLock. Dashboard polls at 1Hz across
// tabs, so call frequency × jobs dominates the CRUD path's allocator pressure.
// Only the 8-byte entryID is pooled; each Job is copied exactly once, straight
// into the caller-owned result (#530, #1118). Callers reset length (`:0`)
// before Put.
var listEntryIDsPool = sync.Pool{
	New: func() any {
		s := make([]cronEntryID, 0, 64)
		return &s
	},
}

// listNextByIDPool reuses the EntryID -> Next time map. `clear()` before
// re-Put so stale keys from a larger snapshot don't leak into a smaller one.
var listNextByIDPool = sync.Pool{
	New: func() any {
		m := make(map[cronEntryID]time.Time, 64)
		return &m
	},
}

// PerChatJobCount returns the number of jobs registered against the
// (Platform, ChatID) chat — O(1) via s.chatJobCount, for dashboard / metrics
// surfaces rendering "N/M cron jobs in this chat" without a ListJobs walk.
// Returns 0 for an unknown chat and on a nil *Scheduler (dashboard renders
// during bootstrap before the scheduler is wired).
func (s *Scheduler) PerChatJobCount(plat, chatID string) int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.chatJobCount[chatKeyFor(plat, chatID)]
}

// ListJobs returns jobs for a specific chat. Walks the jobsByChat index —
// O(jobs-in-chat), not O(all jobs) — since dashboard polls hit this at 1Hz
// per active chat.
func (s *Scheduler) ListJobs(plat, chatID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bucket := s.jobsByChat[chatKeyFor(plat, chatID)]
	// Pre-allocate so an empty result marshals as `[]`, not `null`, matching
	// ListAllJobsWithNextRun and frontend `.length` checks.
	result := make([]Job, 0, len(bucket))
	for _, j := range bucket {
		result = append(result, *j)
	}
	return result
}

// GetJob returns a copy of the job with the given id. The bool is false when
// no such job exists. Read-only; callers that need to mutate go through
// UpdateJob so persistence and cron re-registration stay atomic.
func (s *Scheduler) GetJob(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// JobWithNextRun pairs a Job snapshot with its next scheduled run time so
// callers rendering lists (dashboard) don't need a second round-trip per job.
type JobWithNextRun struct {
	Job     Job
	NextRun time.Time
}

// ListJobsWithNextRun returns the jobs for a specific chat plus each job's
// next scheduled run — the chat-narrowed twin of ListAllJobsWithNextRun, so
// callers need not walk every job to render one chat (#956).
//
// Lock strategy mirrors ListAllJobsWithNextRun: snapshot (Job copy, entryID)
// under s.mu.RLock, release, then read s.cron.Entries() lock-free to avoid
// inverting the cron dispatcher's lock order (cron-internal → execute →
// s.mu.Lock). The result is always non-nil (`[]`) for wire-format symmetry.
func (s *Scheduler) ListJobsWithNextRun(plat, chatID string) []JobWithNextRun {
	// Same pool as ListAllJobsWithNextRun so 1Hz polls pay zero allocs for
	// the transient ids buffer.
	idsPtr := listEntryIDsPool.Get().(*[]cronEntryID)
	ids := (*idsPtr)[:0]
	defer func() {
		*idsPtr = ids[:0]
		listEntryIDsPool.Put(idsPtr)
	}()

	s.mu.RLock()
	bucket := s.jobsByChat[chatKeyFor(plat, chatID)]
	result := make([]JobWithNextRun, 0, len(bucket))
	if cap(ids) < len(bucket) {
		ids = make([]cronEntryID, 0, len(bucket))
	}
	for _, j := range bucket {
		result = append(result, JobWithNextRun{Job: *j})
		ids = append(ids, j.entryID)
	}
	s.mu.RUnlock()

	if len(result) == 0 {
		return result
	}

	// Entries() read outside s.mu (lock-order safe). entryID 0 = paused, keeps
	// zero NextRun. Small buckets take a linear scan per job — cheaper than
	// building a map and avoids touching the shared pool; above
	// listNextRunMapThreshold the jobs × |entries| product wins and we switch
	// to the pooled entryID→Next map (#1583).
	entries := s.cron.Entries()
	if len(result) <= listNextRunMapThreshold {
		for i, id := range ids {
			if id == 0 {
				continue
			}
			for _, e := range entries {
				if e.ID == id {
					result[i].NextRun = e.Next
					break
				}
			}
		}
		return result
	}

	nextByIDPtr := listNextByIDPool.Get().(*map[cronEntryID]time.Time)
	nextByID := *nextByIDPtr
	clear(nextByID)
	defer func() {
		clear(nextByID)
		listNextByIDPool.Put(nextByIDPtr)
	}()
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

// listNextRunMapThreshold is the per-chat job count at or below which
// ListJobsWithNextRun linearly scans Entries() per job instead of building the
// pooled entryID→Next map. 8 is past the common 1-5 jobs/chat bucket, so the
// typical poll stays allocation-free while a job-hoarding chat stays
// linear instead of quadratic (#1583).
const listNextRunMapThreshold = 8

// ListAllJobsWithNextRun returns every job plus its next scheduled run.
// Lock strategy: snapshot (Job copy, entryID) under s.mu.RLock, release, then
// call s.cron.Entries() without holding s.mu — calling it inside s.mu would
// invert the lock order the cron dispatcher takes (cron-internal → execute →
// recordResult → s.mu.Lock). One Entries() snapshot feeds an entryID→Next map,
// O(N) with a single runningMu acquisition, instead of per-job Entry() calls
// that are O(N²) at 1Hz dashboard polling.
func (s *Scheduler) ListAllJobsWithNextRun() []JobWithNextRun {
	// The two transient containers are pooled to keep per-poll allocs flat as
	// job count grows; the caller-owned result slice is NOT pooled, and each
	// Job is copied straight into it under RLock (no second copy).
	idsPtr := listEntryIDsPool.Get().(*[]cronEntryID)
	ids := (*idsPtr)[:0]
	defer func() {
		// Reset length but keep capacity so the next call skips the make.
		*idsPtr = ids[:0]
		listEntryIDsPool.Put(idsPtr)
	}()

	var result []JobWithNextRun
	s.mu.RLock()
	if cap(ids) < len(s.jobs) {
		ids = make([]cronEntryID, 0, len(s.jobs))
	}
	result = make([]JobWithNextRun, 0, len(s.jobs))
	for _, j := range s.jobs {
		// Single Job copy: directly into the caller-owned result. NextRun is
		// patched in by index below once Entries() has been read lock-free.
		result = append(result, JobWithNextRun{Job: *j})
		ids = append(ids, j.entryID)
	}
	s.mu.RUnlock()

	// Single Entries() snapshot → pooled map, `clear()`-ed before re-Put so
	// stale keys from a larger snapshot don't leak. Called outside s.mu (see
	// godoc).
	entries := s.cron.Entries()
	nextByIDPtr := listNextByIDPool.Get().(*map[cronEntryID]time.Time)
	nextByID := *nextByIDPtr
	clear(nextByID)
	defer func() {
		clear(nextByID)
		listNextByIDPool.Put(nextByIDPtr)
	}()
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
