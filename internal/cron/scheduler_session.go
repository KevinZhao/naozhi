// scheduler_session.go: dashboard ↔ cron session-ID exclusion API.
//
// Split out of scheduler.go to keep this small but distinct domain
// (auto-workspace-chain blacklist + history-panel hide list) in one place.
// No behaviour change. Methods stay on *Scheduler so the s.mu / s.jobs /
// s.runningJobs / s.runStore fields remain accessible without exporting.

package cron

import (
	"time"
)

// R20260527122801-ARCH-1 (#1318): The compile-time guard
// `var _ session.SessionIDExcluder = (*Scheduler)(nil)` previously lived
// here, which forced internal/cron to import internal/session — the
// last reverse import in production code (RFC cron-sysession-merge
// Phase B). The guard moved to cmd/naozhi/cron_router_adapter.go where
// session is already imported (alongside the InterruptOutcome ordinal
// pin and the cron.SessionRouter guard), keeping the breakage co-located
// with the wiring that actually consumes it. cron is now fully decoupled
// from session in production code.

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
// Cost: O(1) on a warm cache (mutex + map lookup); O(jobs × recentCap)
// when the cache misses or has expired. R245-GO-4 (#844): previously
// this routed through KnownSessionIDs() which clones the cached set
// before returning a map[string]bool — for a single-key probe that
// allocation is pure overhead (~recentCap × jobs entries copied per
// auto-chain spawn). containsSessionID below reads the cached set
// directly under the cache mutex and short-circuits on the first
// match. Public KnownSessionIDs() retains the clone-and-return shape
// because dashboard pollers iterate the map.
func (s *Scheduler) IsExcluded(sessionID string) bool {
	if s == nil || sessionID == "" {
		return false
	}
	return s.containsSessionID(sessionID)
}

// LookupKnownSessionID reports whether the given Claude sessionID belongs
// to a cron-spawned run, using the same fast-path / TTL-cache pipeline as
// IsExcluded but without going through the session.SessionIDExcluder
// interface boundary. Callers that already hold a *Scheduler reference
// (auto-workspace-chain spawn, dashboard history-panel filter,
// containsSessionID-equivalent probes) avoid the per-call interface
// dispatch + `if s == nil` short-circuit and read the named API exactly
// as proposed in R242-ARCH-23 (#767). Returns false on the empty
// sessionID for shape symmetry with IsExcluded.
//
// The cluster of related issues (R243-PERF-2 / R242-PERF-7) targeted the
// pre-cache O(jobs × recentCap) rebuild that ran on every IsExcluded
// probe. R245-GO-4 already collapsed that walk into containsSessionID's
// fast path (LastSessionID + runningJobs lookup before any rebuild), so
// the perf delta of LookupKnownSessionID over IsExcluded is microseconds
// — the named API is the user-visible payoff.
//
// Safe to call on a nil Scheduler — returns false. The name mirrors
// KnownSessionIDs (the bulk dashboard accessor) so future readers see
// "Lookup" → single-key probe / "Known" → full set in one place.
func (s *Scheduler) LookupKnownSessionID(sessionID string) bool {
	if s == nil || sessionID == "" {
		return false
	}
	return s.containsSessionID(sessionID)
}

// containsSessionID is the single-key probe variant of KnownSessionIDs.
// Shares the TTL cache + invalidation contract — readers see the same
// snapshot a concurrent KnownSessionIDs() caller would observe — but
// avoids the per-call map clone IsExcluded does not need. Triggers a
// rebuild on the same conditions as KnownSessionIDs (cold cache or
// TTL expired); the rebuilt set is then cached so subsequent IsExcluded
// + KnownSessionIDs callers in the same window share work. R245-GO-4.
//
// Fast-path (cache cold, single-key probe): walk Job.LastSessionID under
// s.mu RLock then s.runningJobs.Range — both are O(jobs) and avoid the
// O(jobs × recentCap) runStore.Recent walk that buildKnownSessionsSet
// would otherwise pay for. Only when neither cheap source matches do we
// fall through to the full build (which still populates the TTL cache
// so subsequent IsExcluded + KnownSessionIDs callers see the same
// snapshot). The fast path is intentionally cache-bypassing: it does
// not poison the cache with a partial set, so a subsequent
// KnownSessionIDs() caller still gets the complete history. R245-GO-4
// (#844).
func (s *Scheduler) containsSessionID(sessionID string) bool {
	s.knownSessionsCache.mu.Lock()
	if s.knownSessionsCache.set != nil &&
		time.Since(s.knownSessionsCache.generatedAt) < knownSessionsCacheTTL {
		_, ok := s.knownSessionsCache.set[sessionID]
		s.knownSessionsCache.mu.Unlock()
		return ok
	}
	s.knownSessionsCache.mu.Unlock()

	// Cold cache: cheap fast path before the O(jobs × recentCap) build.
	// Most spawn-time IsExcluded probes target the *just-written*
	// LastSessionID of an active or recently-finished job — both of
	// these are reachable without touching runStore.Recent.
	//
	// R20260527-PERF-5 (#1294) proposed a parallel `lastSessionIDs
	// map[string]struct{}` updated synchronously in recordTerminalResult
	// to avoid the s.jobs walk under RLock. Deferred: the full proposal
	// requires hooking every LastSessionID mutation site (DeleteJobByID,
	// SetJobWorkDir clear path, recordTerminalResult, snapshot replays)
	// AND a ttl cache invalidation tie-in with knownSessionsCache. The
	// O(jobs) walk is RLock-only with an early-break on first match
	// (typical case <50 jobs, ≤10ns/iter), and the auto-workspace-chain
	// caller is rate-limited by user message frequency — paying ~500ns
	// per probe is acceptable until the broader R250-PERF-7 cache
	// refactor lands. Tracking via the issue title.
	s.mu.RLock()
	for _, j := range s.jobs {
		if j.LastSessionID == sessionID {
			s.mu.RUnlock()
			return true
		}
	}
	s.mu.RUnlock()

	found := false
	s.runningJobs.Range(func(_, v any) bool {
		inf, ok := v.(*runInflight)
		if !ok || inf == nil {
			return true
		}
		view, running := inf.snapshot()
		if running && view.SessionID == sessionID {
			found = true
			return false
		}
		return true
	})
	if found {
		return true
	}

	// Not in the cheap sources — pay the full build and populate the
	// TTL cache so subsequent callers (KnownSessionIDs at 1Hz from the
	// dashboard) reuse this work.
	set := s.buildKnownSessionsSet()

	s.knownSessionsCache.mu.Lock()
	s.knownSessionsCache.set = set
	s.knownSessionsCache.generatedAt = time.Now()
	s.knownSessionsCache.mu.Unlock()

	_, ok := set[sessionID]
	return ok
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
// Result is a fresh map; safe to retain.  TTL-cached for
// knownSessionsCacheTTL so dashboard 1Hz pollers do not pay the
// O(jobs × recentCap) build cost on every call. Invalidated on
// LastSessionID writes and runStore.Append. Returns an empty
// (non-nil) map when there are no jobs.
//
// Safe to call on a nil Scheduler — returns empty map.  R245-ARCH
// (cron+sys hide-from-history); R250-PERF-7 (TTL cache).
func (s *Scheduler) KnownSessionIDs() map[string]bool {
	if s == nil {
		return map[string]bool{}
	}

	s.knownSessionsCache.mu.Lock()
	if s.knownSessionsCache.set != nil &&
		time.Since(s.knownSessionsCache.generatedAt) < knownSessionsCacheTTL {
		// Clone to honour the "safe to retain" contract — callers may
		// mutate or persist the returned map.
		cached := s.knownSessionsCache.set
		s.knownSessionsCache.mu.Unlock()
		out := make(map[string]bool, len(cached))
		for id := range cached {
			out[id] = true
		}
		return out
	}
	s.knownSessionsCache.mu.Unlock()

	set := s.buildKnownSessionsSet()

	s.knownSessionsCache.mu.Lock()
	s.knownSessionsCache.set = set
	s.knownSessionsCache.generatedAt = time.Now()
	s.knownSessionsCache.mu.Unlock()

	out := make(map[string]bool, len(set))
	for id := range set {
		out[id] = true
	}
	return out
}

// buildKnownSessionsSet does the actual O(jobs × recentCap) walk that
// KnownSessionIDs serves out of cache. Extracted so the cached and
// uncached paths share one source of truth. R250-PERF-7.
func (s *Scheduler) buildKnownSessionsSet() map[string]struct{} {
	out := make(map[string]struct{}, 32)

	s.mu.RLock()
	jobIDs := make([]string, 0, len(s.jobs))
	for id, j := range s.jobs {
		jobIDs = append(jobIDs, id)
		if j.LastSessionID != "" {
			out[j.LastSessionID] = struct{}{}
		}
	}
	s.mu.RUnlock()

	// In-flight runs may have a SessionID set even before the run
	// terminates (set by setSessionID after GetOrCreate returns).
	s.runningJobs.Range(func(_, v any) bool {
		if inf, ok := v.(*runInflight); ok && inf != nil {
			if view, running := inf.snapshot(); running && view.SessionID != "" {
				out[view.SessionID] = struct{}{}
			}
		}
		return true
	})

	// Persisted history.  Walk recent runs per job (already cached
	// inside runStore).  RunStore is nil only in tests.
	//
	// R20260527-PERF-6 (#1285): use runStore.RecentSessionIDs which reads
	// SessionID strings directly off the cache ring under entry.mu instead
	// of value-copying the full []CronRunSummary (Result field up to ~4 KB
	// per row). With 50 jobs × 200-cap that saves ~10000 summary copies on
	// every cold buildKnownSessionsSet rebuild without changing semantics —
	// the cold disk fallback path is still funneled through the same
	// cache+disk walk Recent uses.
	if s.runStore != nil {
		for _, jobID := range jobIDs {
			for _, sid := range s.runStore.RecentSessionIDs(jobID, knownSessionIDsRecentCap) {
				out[sid] = struct{}{}
			}
		}
	}

	return out
}

// invalidateKnownSessionsCache clears the TTL snapshot so the next
// KnownSessionIDs call rebuilds. Called from mutator paths that can
// change the set: LastSessionID writes (recordResultP0WithSanitised)
// and runStore.Append. Cheap (one mutex + nil-set), so callers can
// invoke unconditionally. R250-PERF-7.
func (s *Scheduler) invalidateKnownSessionsCache() {
	if s == nil {
		return
	}
	s.knownSessionsCache.mu.Lock()
	s.knownSessionsCache.set = nil
	s.knownSessionsCache.mu.Unlock()
}
