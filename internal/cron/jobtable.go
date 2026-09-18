package cron

// jobtable.go — the job registry and its derived indices, with the lock that
// keeps them from drifting apart (docs/rfc/cron-jobtable-extraction.md §3).
//
// The four fields are four views of one invariant: jobs is the source of truth,
// chatJobCount is its per-chat cardinality, jobsByChat its per-chat index, and
// sortedJobIDs its key order. A writer that updates one and not the rest leaves
// the per-chat cap counting jobs that no longer exist, or persist writing a job
// set that omits one. They move together or not at all, which is why they share
// a lock and now share a type.
//
// saveSeq lives here too, and that is not incidental: its whole purpose is to
// total-order marshaled snapshots against the state they represent, so it has to
// be assigned inside the same critical section that takes the snapshot. Leaving
// it on Scheduler while the lock moved here would silently break that ordering —
// two snapshots could take their seq under a lock that no longer serialises them
// against each other.
//
// The API hands out DATA: a value snapshot, a count, an existence answer. It
// never returns the lock, and never a live *Job whose fields a caller could read
// after the lock is gone. Every escape hatch that exists today — a closure run
// under the write lock, a live pointer surviving past RUnlock — is a way for the
// invariant to be broken from outside the one file that maintains it.
//
// MIGRATION, TEMPORARY: jobTable is embedded in Scheduler rather than held as a
// named field, so s.jobs / s.mu still resolve for the ~560 test lines that reach
// straight into the registry. That promotion is a scaffold, not the design: the
// step that migrates those tests onto export_test.go ports also un-embeds this,
// and only then is the encapsulation above actually enforced rather than merely
// offered.

import (
	"sync"
	"sync/atomic"
)

type jobTable struct {
	// mu guards the four fields below (RLock reads, Lock writes). It does NOT
	// cover Scheduler's immutable-config fields (read lock-free) nor its
	// independently-synchronised ones (runningJobs / telemetry / store* / *Once),
	// each of which carries its own primitive.
	mu sync.RWMutex
	// jobs is the source of truth. All reads and writes hold mu.
	jobs map[string]*Job
	// chatJobCount tracks jobs per (Platform, ChatID). Maintained with jobs
	// writes under mu so the per-chat capacity check is O(1); entries are
	// deleted at zero so the working set tracks live chats.
	chatJobCount map[chatJobKey]int
	// jobsByChat indexes *Job by (Platform, ChatID) so findByPrefixLocked scans
	// only the caller's chat (~O(5)) instead of all of jobs (O(500)). Maintained
	// with jobs writes under mu; entries deleted when the slice empties.
	// (Platform, ChatID) is immutable post-AddJob, so an entry never moves across
	// keys — add appends, delete swaps-and-shrinks.
	jobsByChat map[chatJobKey][]*Job
	// sortedJobIDs mirrors the keys of jobs in ascending ID order, maintained
	// incrementally at the two seams that mutate jobs (addToChatIndexLocked /
	// deleteJobLocked) so persist avoids an O(N log N) sort under mu. jobs stays
	// the source of truth: marshalJobsLocked validates this hint and rebuilds
	// from the map on drift.
	sortedJobIDs []string
	// saveSeq tags every marshaled snapshot at capture time, under mu.
	// saveMarshaled skips the write under storeMu if lastSavedSeq is already
	// newer: sync.Mutex is not FIFO, so an older snapshot could otherwise reach
	// storeMu after a newer one and overwrite it on disk.
	saveSeq atomic.Uint64
}

func newJobTable() jobTable {
	return jobTable{
		jobs:         make(map[string]*Job),
		chatJobCount: make(map[chatJobKey]int),
		jobsByChat:   make(map[chatJobKey][]*Job),
	}
}

// ── reads: values only ──────────────────────────────────────────────────────

// exists reports whether id is registered.
func (t *jobTable) exists(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.jobs[id]
	return ok
}

// count returns the number of registered jobs.
func (t *jobTable) count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.jobs)
}

// countForChat returns how many jobs are registered for one chat. This is the
// cap check's O(1) answer, and it comes from chatJobCount rather than a scan of
// jobs precisely so the two cannot disagree without this file noticing.
func (t *jobTable) countForChat(k chatJobKey) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.chatJobCount[k]
}

// ids returns the registered ids in ascending order. A copy: the caller may hold
// it past the lock, which is the whole point of returning data.
func (t *jobTable) ids() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, len(t.sortedJobIDs))
	copy(out, t.sortedJobIDs)
	return out
}

// liveness reports whether id is registered and, if so, whether it is paused.
// One call rather than exists + a second lookup: the two answers have to come
// from the same lock hold, or a pause landing between them turns "live" into a
// run that should have been skipped.
func (t *jobTable) liveness(id string) (registered, paused bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	j, ok := t.jobs[id]
	return ok, ok && j.Paused
}

// lastSessionID returns the registered job's current LastSessionID. A value, so
// the caller cannot go on reading other fields of a *Job it no longer has the
// lock for.
func (t *jobTable) lastSessionID(id string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	j, ok := t.jobs[id]
	if !ok {
		return "", false
	}
	return j.LastSessionID, true
}

// nextSaveSeq assigns the next snapshot sequence. Callers that already hold mu
// use saveSeq.Add directly; this exists for the paths that do not.
func (t *jobTable) nextSaveSeq() uint64 { return t.saveSeq.Add(1) }
