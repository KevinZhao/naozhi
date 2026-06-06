// scheduler_jobs_list.go: cron Job read-only list / snapshot surface.
//
// Holds the lock-free read path split out of scheduler_jobs.go:
// PerChatJobCount, ListJobs, ListJobsWithNextRun, ListAllJobsWithNextRun,
// the JobWithNextRun result type, the listNextRunMapThreshold linear-scan
// vs pooled-map switch, and the two sync.Pool scratch containers
// (listEntryIDsPool / listNextByIDPool) that only this cluster uses.
//
// No behaviour change. Methods stay on *Scheduler so private fields
// remain accessible without exporting.

package cron

import (
	"sync"
	"time"
)

// listEntryIDsPool reuses the transient []cronEntryID scratch slice
// recorded during ListAllJobsWithNextRun's two-phase snapshot. Dashboard
// polls at 1Hz across multiple tabs so the call frequency × jobs-per-call
// dominates the cron CRUD path's allocator pressure. Get returns a
// zero-length slice with potentially non-zero capacity — callers must
// reset length (`:0`) when they Put it back. R247-PERF-4 (#530).
//
// R250-PERF-15 (#1118): previously this pooled a []listSnapshotPair whose
// element embedded a full Job value-copy (~300B incl. RunCounters), so the
// snapshot copied every Job twice — once into the pooled scratch under
// RLock, then again into the caller-owned result slice. With N jobs × N
// tabs × 1Hz that wasted ~750KB/s of GC churn on the redundant copy. We
// now copy each Job exactly once (directly into result under RLock) and the
// pooled scratch holds only the cheap entryID (8 bytes) needed to patch
// NextRun after s.cron.Entries() runs lock-free.
var listEntryIDsPool = sync.Pool{
	New: func() any {
		s := make([]cronEntryID, 0, 64)
		return &s
	},
}

// listNextByIDPool reuses the EntryID -> Next time map. Reset via
// `clear()` (Go 1.21) before re-Put so stale keys from a larger previous
// snapshot don't leak into a smaller current one. R247-PERF-4 (#530).
var listNextByIDPool = sync.Pool{
	New: func() any {
		m := make(map[cronEntryID]time.Time, 64)
		return &m
	},
}

// PerChatJobCount returns the number of jobs registered against the
// (Platform, ChatID) chat. Backed by s.chatJobCount (R237-PERF-5 / #661):
// O(1) read, lock-free outside the RLock window, vs the historical
// O(N) scan that addJobAcquiringLock used to enforce maxJobsPerChat.
//
// Intended use: dashboard / metrics surfaces that want to render
// "you have N/M cron jobs in this chat" without paying the cost of a
// full ListJobs walk. Returns 0 for a chat with no registered jobs.
//
// Safe on a nil *Scheduler (returns 0) so dashboard renders during
// the bootstrap window before the scheduler is wired do not NPE.
func (s *Scheduler) PerChatJobCount(plat, chatID string) int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.chatJobCount[chatKeyFor(plat, chatID)]
}

// ListJobs returns jobs for a specific chat.
//
// R20260527-PERF-1: walks the jobsByChat[(plat, chatID)] index instead of
// scanning the entire s.jobs map — O(jobs-in-chat) vs the historical O(N).
// Matters once a deployment accumulates many cron jobs across many chats:
// dashboard list polls hit ListJobs at 1 Hz per active chat.
func (s *Scheduler) ListJobs(plat, chatID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bucket := s.jobsByChat[chatKeyFor(plat, chatID)]
	// R247-GO-3: pre-allocate so an empty result marshals as `[]` instead of
	// `null` — keeps the JSON wire-format consistent with ListAllJobsWithNextRun
	// and frontend `.length` defenders unaffected. [BREAKING-LOCAL]
	result := make([]Job, 0, len(bucket))
	for _, j := range bucket {
		result = append(result, *j)
	}
	return result
}

// JobWithNextRun pairs a Job snapshot with its next scheduled run time so
// callers rendering lists (dashboard) don't need a second round-trip per job.
type JobWithNextRun struct {
	Job     Job
	NextRun time.Time
}

// ListJobsWithNextRun returns the jobs for a specific chat plus each job's
// next scheduled run — the chat-narrowed twin of ListAllJobsWithNextRun.
//
// R249-CR-12 (#956): ListJobs returns chat-scoped []Job WITHOUT NextRun, while
// ListAllJobsWithNextRun returns NextRun but for EVERY job. A caller wanting
// "this chat's jobs + their NextRun" previously had to call the all-jobs
// variant and filter — paying an O(allJobs) walk to render an O(jobs-in-chat)
// list. This helper closes that asymmetry: it walks the jobsByChat bucket
// (O(jobs-in-chat)) and patches NextRun in.
//
// Lock strategy mirrors ListAllJobsWithNextRun: snapshot (Job copy, entryID)
// under s.mu.RLock, release s.mu, then read s.cron.Entries() lock-free to
// avoid inverting the cron dispatcher's lock order (cron-internal → execute →
// s.mu.Lock). The result slice is always non-nil (empty bucket → `[]`) for
// wire-format symmetry with ListJobs / ListAllJobsWithNextRun.
func (s *Scheduler) ListJobsWithNextRun(plat, chatID string) []JobWithNextRun {
	// R20260602-CR-3: reuse listEntryIDsPool for the per-chat entryID
	// scratch slice — the same pool ListAllJobsWithNextRun uses — so 1Hz
	// dashboard polls pay zero allocs for the transient ids buffer.
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

	// Single Entries() snapshot read outside s.mu (lock-order safe). entryID 0
	// means the job is not registered with cron (paused) and keeps the zero
	// NextRun.
	//
	// R20260602141221-PERF-2 (#1583): the lookup is O(jobs-in-chat × |entries|)
	// — a linear scan of the full Entries() snapshot per job. For the common
	// per-chat bucket (1-5 jobs) the scan is cheaper than building a map: a
	// handful of scans over |entries| beats paying the map's hashing + (even
	// pooled) bookkeeping, and avoids touching the shared listNextByIDPool. But
	// when a chat accumulates many jobs the product blows up (jobs × |entries|,
	// |entries|≤500) and the dashboard's 1Hz poll amplifies it. Above the
	// threshold we switch to the same pooled entryID→Next map ListAllJobsWithNextRun
	// uses — O(|entries|) build + O(1) per job — so both the small-bucket and
	// large-bucket cases stay cheap. The threshold is deliberately small: by a
	// handful of jobs the map's single Entries() walk already wins on the
	// comparison count and the constant-factor crossover is well behind us.
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
// ListJobsWithNextRun uses a direct linear Entries() scan rather than building
// the pooled entryID→Next map. Below it the scan's small constant factor wins;
// above it the O(jobs × |entries|) product makes the O(|entries|) map build
// cheaper. R20260602141221-PERF-2 (#1583). 8 is comfortably past the common
// 1-5 jobs/chat bucket so the typical dashboard poll keeps the allocation-free
// linear path, while a chat that hoards jobs no longer scales quadratically.
const listNextRunMapThreshold = 8

// ListAllJobsWithNextRun returns every job plus its next scheduled run.
// Lock strategy: snapshot (*Job, entryID) under s.mu.RLock, release s.mu, then
// call s.cron.Entries() without holding s.mu. Calling cron.Entries inside
// s.mu would invert the lock order taken by the cron dispatcher path
// (cron-internal → execute → recordResultP0WithSanitised → s.mu.Lock),
// risking a deadlock.
//
// R236-PERF-11: this used to call s.cron.Entry(id) per job, but
// robfig/cron v3's Entry is implemented as `for _, e := range Entries()
// { if e.ID == id }` and Entries() takes runningMu — so N jobs cost
// N×N entry walks plus N mutex acquisitions on the dispatcher's mutex.
// Building one entryID→Next map up front collapses the cost to O(N) and
// a single mutex acquisition, which matters when the dashboard list API
// polls at 1 Hz with 50 jobs registered.
func (s *Scheduler) ListAllJobsWithNextRun() []JobWithNextRun {
	// R247-PERF-4 (#530) / R250-PERF-15 (#1118): the dashboard list endpoint
	// polls this at 1Hz across N open tabs, so we pool the two transient
	// containers (entryID scratch + EntryID-keyed nextRun map) to keep
	// per-poll alloc count flat as job count grows. The result slice is owned
	// by the caller and stays heap-resident, so it is NOT pooled — and each
	// Job is copied straight into it under RLock so there is no second copy.
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

	// Single Entries() snapshot → entryID-keyed map. The map is pooled and
	// `clear()`-ed before re-Put so stale keys from a previous larger
	// snapshot do not leak into a smaller current one. The alternative —
	// re-walking Entries per job — is O(N²) and re-acquires runningMu per
	// Entry() call. Called outside s.mu to avoid inverting the lock order
	// the cron dispatcher takes (cron-internal → execute → s.mu.Lock).
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
