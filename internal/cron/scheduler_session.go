// scheduler_session.go: dashboard ↔ cron session-ID exclusion API.
//
// Split out of scheduler.go to keep this small but distinct domain
// (auto-workspace-chain blacklist + history-panel hide list) in one place.
// No behaviour change. Methods stay on *Scheduler so the s.mu / s.jobs /
// s.runningJobs / s.runStore fields remain accessible without exporting.

package cron

import "sync"

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
//
// R247-PERF-3 (#529): the per-call O(jobs × recentCap) rebuild flagged
// in the original anchor is now collapsed by the knownSessionsCache TTL
// snapshot (see KnownSessionIDs / containsSessionID). Both the
// dashboard 1Hz path and the spawn-time IsExcluded probe read from the
// same cached set, so this cap only governs the cold-cache rebuild
// frequency. The constant stays here so future tuning lives at the
// boundary it actually controls.
const knownSessionIDsRecentCap = 200

// jobIDsScratchPool reuses the []string scratch slice that
// buildKnownSessionsSet allocates under s.mu RLock to collect job IDs,
// then walks after RUnlock to call runStore.RecentSessionIDs per job.
// invalidateKnownSessionsCache is called on every runStore.Append and
// LastSessionID write, so cold rebuilds can be frequent in busy
// deployments. Pooling the scratch slice removes the per-rebuild
// backing-array allocation without changing semantics: the slice is
// only used inside buildKnownSessionsSet and is returned to the pool
// after all runStore reads are done. R20260603-010128-PERF-1.
var jobIDsScratchPool = sync.Pool{
	New: func() any {
		// Default seed sized for the common 50-job case.
		s := make([]string, 0, 64)
		return &s
	},
}

// jobIDsScratchCapDrop is the cap threshold above which Put refuses to
// recycle the scratch slice, mirroring the marshalEntriesCapDrop policy
// in scheduler_persist.go. Prevents a one-off burst from pinning a
// large backing array indefinitely.
const jobIDsScratchCapDrop = 4 * maxJobsHardCap // 2000 string slots

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
	if set, ok := s.knownSessionsCache.lookupFresh(); ok {
		_, hit := set[sessionID]
		return hit
	}

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
	s.rangeRunningSessionIDs(func(sid string) bool {
		if sid == sessionID {
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
	s.knownSessionsCache.publish(set)

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
// The returned map is READ-ONLY and shared: callers MUST NOT mutate or
// persist it. The set is published once by buildKnownSessionsSet and then
// only ever replaced wholesale (never mutated in place) or dropped by
// invalidateKnownSessionsCache, so handing out the cached map directly is
// race-free for read-only consumers. TTL-cached for knownSessionsCacheTTL
// so dashboard 1Hz pollers do not pay the O(jobs × recentCap) build cost —
// nor an O(N) clone — on every call. Invalidated on LastSessionID writes
// and runStore.Append. Returns an empty (non-nil) map when there are no jobs.
//
// R20260601-PERF-1 (#1544): the previous shape cloned the cached set into a
// fresh map[string]bool on every call (including warm cache hits) to honour
// a "safe to retain" contract. The only production consumer (dashboard
// historyFilter.SkipSessionID) is read-only, so the contract was tightened
// to read-only and the per-call O(N) allocation+copy on the /api/sessions
// 1Hz hot path was removed.
//
// Safe to call on a nil Scheduler — returns empty map.  R245-ARCH
// (cron+sys hide-from-history); R250-PERF-7 (TTL cache).
func (s *Scheduler) KnownSessionIDs() map[string]struct{} {
	if s == nil {
		return map[string]struct{}{}
	}

	if set, ok := s.knownSessionsCache.lookupFresh(); ok {
		return set
	}

	set := s.buildKnownSessionsSet()
	s.knownSessionsCache.publish(set)

	return set
}

// buildKnownSessionsSet does the actual O(jobs × recentCap) walk that
// KnownSessionIDs serves out of cache. Extracted so the cached and
// uncached paths share one source of truth. R250-PERF-7.
func (s *Scheduler) buildKnownSessionsSet() map[string]struct{} {
	// Get a pooled scratch slice for job IDs; reset length to 0 before use.
	jobIDsPtr := jobIDsScratchPool.Get().(*[]string)
	jobIDs := (*jobIDsPtr)[:0]

	s.mu.RLock()
	// R20260603-PERF-3: size the map from len(s.jobs) under the lock already
	// held here, replacing the fixed cap of 32 that caused repeated rehashes
	// for schedulers with many jobs. Each job contributes at most one
	// LastSessionID entry in this loop, so len(s.jobs)+8 is a tight upper
	// bound with a small slack for in-flight and recent-history entries.
	out := make(map[string]struct{}, len(s.jobs)+8)
	for id, j := range s.jobs {
		jobIDs = append(jobIDs, id)
		if j.LastSessionID != "" {
			out[j.LastSessionID] = struct{}{}
		}
	}
	s.mu.RUnlock()

	// In-flight runs may have a SessionID set even before the run
	// terminates (set by setSessionID after GetOrCreate returns).
	s.rangeRunningSessionIDs(func(sid string) bool {
		out[sid] = struct{}{}
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
	if s.runStoreEnabled() {
		for _, jobID := range jobIDs {
			for _, sid := range s.recentSessionIDs(jobID, knownSessionIDsRecentCap) {
				out[sid] = struct{}{}
			}
		}
	}

	// Return scratch slice to pool. jobIDs is used above (runStore loop);
	// Put only after all reads are complete. Drop oversize slices to avoid
	// pinning a burst-inflated backing array. R20260603-010128-PERF-1.
	if cap(jobIDs) <= jobIDsScratchCapDrop {
		// Clear string references to prevent pinning stale job ID strings.
		clear(jobIDs)
		*jobIDsPtr = jobIDs[:0]
		jobIDsScratchPool.Put(jobIDsPtr)
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
	s.knownSessionsCache.invalidate()
}
