package cron

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
