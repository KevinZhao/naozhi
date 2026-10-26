// scheduler_session.go: dashboard ↔ cron session-ID exclusion API.

package cron

// knownSessionIDsRecentCap bounds how many recent runs per job we walk when
// building the known-IDs set. Cron JSONLs are co-located with regular dashboard
// sessions under ~/.claude/projects/<workspace>/, so hiding them from the
// history panel works per-session-ID; recentCap covers the full history-panel
// window (200 entries × 7d) without rereading every JSONL. Busier crons accept
// that older rotations may briefly resurface until their JSONL ages out.
const knownSessionIDsRecentCap = 200

// KnownSessionIDs returns the set of Claude session IDs (UUID-style) spawned
// by cron jobs known to this Scheduler. The dashboard history panel uses it as
// a blacklist so cron-spawned JSONLs stay out of the catch-all "recent
// sessions" list. Sources: Job.LastSessionID for every job, in-flight runs, and
// the last knownSessionIDsRecentCap runs per job from the run store.
//
// The set is built fresh on every call (the dashboard history cache upstream
// already coalesces calls). Returns an empty (non-nil) map on a nil Scheduler
// or no jobs.
func (s *Scheduler) KnownSessionIDs() map[string]struct{} {
	if s == nil {
		return map[string]struct{}{}
	}
	return s.buildKnownSessionsSet()
}

// buildKnownSessionsSet does the O(jobs × recentCap) walk behind
// KnownSessionIDs.
func (s *Scheduler) buildKnownSessionsSet() map[string]struct{} {
	// Allocate the map before the lock window, so make() does not run inside
	// it; a fixed initial capacity trades a few rehashes for a shorter hold.
	out := make(map[string]struct{}, 32)
	jobIDs := s.tbl.sessionIDs(nil, out)

	// In-flight runs may have a SessionID set even before the run
	// terminates (set by setSessionID after GetOrCreate returns).
	s.rangeRunningSessionIDs(func(sid string) bool {
		out[sid] = struct{}{}
		return true
	})

	// Persisted history: RecentSessionIDs reads SessionID strings straight off
	// the cache ring instead of value-copying full []CronRunSummary rows
	// (~4 KB each). The store is disabled only in tests.
	if s.runs.Enabled() {
		for _, jobID := range jobIDs {
			for _, sid := range s.runs.RecentSessionIDs(jobID, knownSessionIDsRecentCap) {
				out[sid] = struct{}{}
			}
		}
	}

	return out
}
