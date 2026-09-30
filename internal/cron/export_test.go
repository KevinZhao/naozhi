package cron

import "testing"

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
