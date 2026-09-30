package cron

import (
	"testing"
	"time"
)

// export_test.go — test-only clamp ports into the un-embedded registry and
// gate (docs/rfc/cron-jobtable-extraction.md §8.2).
//
// These are NOT an API. Production code never sees them (the compiler
// excludes _test.go), which is the entire point: jobTable's contract is that
// its lock and live *Job pointers never escape, and the tests that need to
// build inconsistent states or clamp the lock get this one explicit,
// greppable back door instead of field promotion quietly offering them
// everything. A grep for `tblForTest\|gateForTest` enumerates every test
// that reaches past the data-only API.

// tblForTest exposes the registry — its lock, its maps, its indices — for
// tests that set up state directly or clamp mu to prove lock discipline.
func (s *Scheduler) tblForTest() *jobTable { return &s.tbl }

// gateForTest exposes the per-job execution slot for tests that inspect
// runningJobs or hold a shard mutex to prove serialisation.
func (s *Scheduler) gateForTest() *runGate { return &s.gate }

// putJobForTest registers j as-is under its own ID — no ID generation, no
// persist, no robfig entry — indexed the way a load indexes it. For tests that
// seed a job AddJob would reject or would register differently.
func (s *Scheduler) putJobForTest(j *Job) {
	s.tbl.mu.Lock()
	defer s.tbl.mu.Unlock()
	s.tbl.jobs[j.ID] = j
	s.tbl.indexLocked(j)
}

// editJobForTest applies fn to registered job id under the table lock, and
// fails the test when no such job is registered.
func (s *Scheduler) editJobForTest(t testing.TB, id string, fn func(*Job)) {
	t.Helper()
	s.tbl.mu.Lock()
	defer s.tbl.mu.Unlock()
	j, ok := s.tbl.jobs[id]
	if !ok {
		t.Fatalf("editJobForTest: no job %q", id)
	}
	fn(j)
}

// jobForTest returns a copy of registered job id, and fails the test when no
// such job is registered.
func (s *Scheduler) jobForTest(t testing.TB, id string) Job {
	t.Helper()
	j, ok := s.tbl.snapshot(id)
	if !ok {
		t.Fatalf("jobForTest: no job %q", id)
	}
	return j
}

// dropJobForTest unregisters job id the way a delete does in memory — map and
// indices — without the robfig Remove or the runs cleanup, for tests that
// simulate a job vanishing mid-run.
func (s *Scheduler) dropJobForTest(id string) {
	s.tbl.mu.Lock()
	defer s.tbl.mu.Unlock()
	if j, ok := s.tbl.jobs[id]; ok {
		s.tbl.deleteLocked(j)
	}
}

// findByPrefixForTest resolves idPrefix within (plat, chatID) under the table
// lock, as the prefix-scoped mutations do.
func (s *Scheduler) findByPrefixForTest(idPrefix, plat, chatID string) (*Job, error) {
	s.tbl.mu.RLock()
	defer s.tbl.mu.RUnlock()
	return s.tbl.findByPrefixLocked(idPrefix, plat, chatID)
}

// marshalForTest serialises the job set under the table lock, the way a
// persist does.
func (s *Scheduler) marshalForTest() ([]byte, error) {
	s.tbl.mu.RLock()
	defer s.tbl.mu.RUnlock()
	return s.tbl.marshalLocked()
}

// snapshotForSaveForTest takes the off-lock persist snapshot under the table
// lock, as recordTerminalResult does.
func (s *Scheduler) snapshotForSaveForTest() jobsSnapshot {
	s.tbl.mu.Lock()
	defer s.tbl.mu.Unlock()
	return s.tbl.snapshotForSaveLocked()
}

// mapKeysForTest returns the registry's map keys, unordered — the source of
// truth the sorted-ID index is checked against.
func (s *Scheduler) mapKeysForTest() []string {
	s.tbl.mu.RLock()
	defer s.tbl.mu.RUnlock()
	keys := make([]string, 0, len(s.tbl.jobs))
	for id := range s.tbl.jobs {
		keys = append(keys, id)
	}
	return keys
}

// registerJobForTest plans, commits and applies j's cron entry in one call.
// Safe only before s.cron.Start(), when robfig's Schedule appends to a slice
// instead of rendezvousing with the run loop; tests use it to seed an entry
// on a job they built by hand.
func (s *Scheduler) registerJobForTest(j *Job) error {
	p, err := planCronEntry(j.ID, j.Schedule, time.Now())
	if err != nil {
		return err
	}
	applyCronEntry(j, p, s.commitCronEntry(p))
	return nil
}

// chatCountsForTest returns, from one lock hold, the per-chat counter and a
// recount grouped from the job map, so the two can be checked against each
// other. Both are copies.
func (s *Scheduler) chatCountsForTest() (counter, recount map[chatJobKey]int) {
	s.tbl.mu.RLock()
	defer s.tbl.mu.RUnlock()
	counter = make(map[chatJobKey]int, len(s.tbl.chatJobCount))
	for k, v := range s.tbl.chatJobCount {
		counter[k] = v
	}
	recount = make(map[chatJobKey]int, len(s.tbl.jobs))
	for _, j := range s.tbl.jobs {
		recount[chatKeyFor(j.Platform, j.ChatID)]++
	}
	return counter, recount
}

// chatIndexForTest returns, from one lock hold, the per-chat index as job IDs
// and a regrouping of the job map, so the two can be checked against each
// other. Both are copies.
func (s *Scheduler) chatIndexForTest() (index map[chatJobKey][]string, regroup map[chatJobKey]map[string]bool) {
	s.tbl.mu.RLock()
	defer s.tbl.mu.RUnlock()
	index = make(map[chatJobKey][]string, len(s.tbl.jobsByChat))
	for k, list := range s.tbl.jobsByChat {
		ids := make([]string, len(list))
		for i, j := range list {
			ids[i] = j.ID
		}
		index[k] = ids
	}
	regroup = make(map[chatJobKey]map[string]bool, len(s.tbl.jobs))
	for _, j := range s.tbl.jobs {
		k := chatKeyFor(j.Platform, j.ChatID)
		if regroup[k] == nil {
			regroup[k] = make(map[string]bool)
		}
		regroup[k][j.ID] = true
	}
	return index, regroup
}

// putUnindexedJobForTest writes j into the job map and nothing else, leaving
// the per-chat and sorted-ID indices behind — the drift the sorted-ID fallback
// and the index-consistency checks exist to survive.
func (s *Scheduler) putUnindexedJobForTest(j *Job) {
	s.tbl.mu.Lock()
	defer s.tbl.mu.Unlock()
	s.tbl.jobs[j.ID] = j
}
