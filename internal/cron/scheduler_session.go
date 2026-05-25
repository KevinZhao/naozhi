// scheduler_session.go: dashboard ↔ cron session-ID exclusion API.
//
// Split out of scheduler.go to keep this small but distinct domain
// (auto-workspace-chain blacklist + history-panel hide list) in one place.
// No behaviour change. Methods stay on *Scheduler so the s.mu / s.jobs /
// s.runningJobs / s.runStore fields remain accessible without exporting.

package cron

import "github.com/naozhi/naozhi/internal/session"

// Compile-time guard: *Scheduler must satisfy session.SessionIDExcluder.
// If session.SessionIDExcluder gains a method, this assertion makes the
// breakage land here — next to the implementation — instead of at a
// distant call site like router.AddSessionIDExcluder.
var _ session.SessionIDExcluder = (*Scheduler)(nil)

// knownSessionIDsRecentCap bounds how many recent runs per job we walk
// when building the known-IDs set. Cron jobs share the user's workspace
// (~/.claude/projects/<workspace>/<UUID>.jsonl is co-located with regular
// dashboard sessions), so the only way to hide cron-spawned JSONLs from
// the history panel is per-session-ID. We pull `recentCap` runs per job
// — enough to cover the full history-panel window (200 entries × 7d).
// Walking the full per-job ring would reread every JSONL on every poll
// (handleList is hit at 1Hz × N tabs); ahead-of-time bounded scan
// matches the dashboard's display cap. Operators with very busy crons
// (more than recentCap distinct SessionIDs in 7 days) accept that older
// rotations may briefly resurface in history until their JSONL ages out.
const knownSessionIDsRecentCap = 200

// IsExcluded reports whether the given Claude sessionID belongs to a
// cron-spawned run. Implements session.SessionIDExcluder so the
// auto-workspace-chain feature can reject cron sessionIDs from the
// candidate pool when filling user sessions' prev_session_ids
// (docs/rfc/auto-workspace-chain.md §4.3 Arch-B2). Returns false for
// the empty sessionID. Safe to call on a nil Scheduler (returns
// false).
//
// Cost: O(jobs × recentCap) per call — KnownSessionIDs builds a
// transient map on every invocation. The auto-chain spawn path calls
// this at most once per spawn so the cost is amortised; dashboard
// 1Hz / hot-path callers should batch via KnownSessionIDs() and reuse
// the snapshot. R247-PERF-3 tracks the long-term TTL-cache fix.
func (s *Scheduler) IsExcluded(sessionID string) bool {
	if s == nil || sessionID == "" {
		return false
	}
	// KnownSessionIDs returns a fresh map; the lookup is O(1) once built.
	return s.KnownSessionIDs()[sessionID]
}

// KnownSessionIDs returns the set of Claude session IDs (UUID-style)
// that have been spawned by cron jobs known to this Scheduler.  The
// dashboard history panel uses this as a session-ID blacklist so
// cron-spawned JSONLs are hidden from the catch-all "recent sessions"
// list (cron has its own 「定时任务」panel for inspection).
//
// Sources, in order of cost:
//
//   - All Job.LastSessionID values held in s.jobs (one per job, cheap).
//   - All in-flight runs (s.runningJobs sync.Map; one per active run).
//   - The last knownSessionIDsRecentCap runs per job from runStore.
//
// Result is a fresh map; safe to retain.  Cost is O(jobs ×
// knownSessionIDsRecentCap), bounded by maxJobsHardCap (500) ×
// recentCap (200) = 100k map ops worst case — acceptable for a
// 30-second-cached dashboard call.  Returns an empty (non-nil) map
// when there are no jobs.
//
// Safe to call on a nil Scheduler — returns empty map.  R245-ARCH
// (cron+sys hide-from-history).
func (s *Scheduler) KnownSessionIDs() map[string]bool {
	if s == nil {
		return map[string]bool{}
	}
	out := make(map[string]bool, 32)

	s.mu.RLock()
	jobIDs := make([]string, 0, len(s.jobs))
	for id, j := range s.jobs {
		jobIDs = append(jobIDs, id)
		if j.LastSessionID != "" {
			out[j.LastSessionID] = true
		}
	}
	s.mu.RUnlock()

	// In-flight runs may have a SessionID set even before the run
	// terminates (set by setSessionID after GetOrCreate returns).
	s.runningJobs.Range(func(_, v any) bool {
		if inf, ok := v.(*runInflight); ok && inf != nil {
			if view, running := inf.snapshot(); running && view.SessionID != "" {
				out[view.SessionID] = true
			}
		}
		return true
	})

	// Persisted history.  Walk recent runs per job (already cached
	// inside runStore).  RunStore is nil only in tests.
	if s.runStore != nil {
		for _, jobID := range jobIDs {
			for _, sum := range s.runStore.Recent(jobID, knownSessionIDsRecentCap) {
				if sum.SessionID != "" {
					out[sum.SessionID] = true
				}
			}
		}
	}

	return out
}
