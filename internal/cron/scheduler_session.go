// scheduler_session.go: dashboard ↔ cron session-ID exclusion API.

package cron

import "sync"

// knownSessionIDsRecentCap bounds how many recent runs per job we walk when
// building the known-IDs set. Cron JSONLs are co-located with regular dashboard
// sessions under ~/.claude/projects/<workspace>/, so hiding them from the
// history panel works per-session-ID; recentCap covers the full history-panel
// window (200 entries × 7d) without rereading every JSONL. Busier crons accept
// that older rotations may briefly resurface until their JSONL ages out. The
// knownSessionsCache TTL snapshot means this cap only governs cold-rebuild cost.
const knownSessionIDsRecentCap = 200

// jobIDsScratchPool reuses the []string scratch slice buildKnownSessionsSet
// fills under s.mu RLock and walks after RUnlock; cold rebuilds can be frequent
// in busy deployments, so pooling removes the per-rebuild backing-array alloc.
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

// LookupKnownSessionID reports whether the given Claude sessionID belongs
// to a cron-spawned run. Callers that hold a *Scheduler reference use this
// single-key probe directly rather than iterating KnownSessionIDs. Returns
// false on the empty sessionID and on a nil Scheduler.
//
// Cost: O(1) on a warm cache (mutex + map lookup); O(jobs × recentCap)
// when the cache misses or has expired.
func (s *Scheduler) LookupKnownSessionID(sessionID string) bool {
	if s == nil || sessionID == "" {
		return false
	}
	return s.containsSessionID(sessionID)
}

// containsSessionID is the single-key probe variant of KnownSessionIDs: it
// shares the TTL cache + invalidation contract but avoids the per-call map
// clone. On a cold cache it first checks the cheap sources (Job.LastSessionID,
// running inflights) and then still builds + publishes the full set, because a
// steady stream of fast-path-only probes would otherwise leave the cache
// permanently cold and force the dashboard's 1Hz KnownSessionIDs() to
// cold-rebuild every tick (#1978).
func (s *Scheduler) containsSessionID(sessionID string) bool {
	if set, ok := s.knownSessionsCache.lookupFresh(); ok {
		_, hit := set[sessionID]
		return hit
	}

	// Cold cache: cheap fast path before the O(jobs × recentCap) build. Most
	// spawn-time probes target the just-written LastSessionID of an active or
	// recently-finished job, reachable without touching runStore.Recent.
	fastHit := false
	s.mu.RLock()
	for _, j := range s.jobs {
		if j.LastSessionID == sessionID {
			fastHit = true
			break
		}
	}
	s.mu.RUnlock()

	if !fastHit {
		s.rangeRunningSessionIDs(func(sid string) bool {
			if sid == sessionID {
				fastHit = true
				return false
			}
			return true
		})
	}

	// Warm the TTL cache even on a fast-path hit (#1978); the extra cost over
	// the cheap sources above is only the runStore.Recent walk. Snapshot the
	// cache generation BEFORE the build reads source data so a concurrent
	// invalidate() cannot be clobbered by our publish (#1811); the local `set`
	// is still consulted even if publish is rejected.
	buildGen := s.knownSessionsCache.beginBuild()
	set := s.buildKnownSessionsSet()
	s.knownSessionsCache.publish(set, buildGen)

	if fastHit {
		return true
	}
	_, ok := set[sessionID]
	return ok
}

// KnownSessionIDs returns the set of Claude session IDs (UUID-style) spawned
// by cron jobs known to this Scheduler. The dashboard history panel uses it as
// a blacklist so cron-spawned JSONLs stay out of the catch-all "recent
// sessions" list. Sources: Job.LastSessionID for every job, in-flight runs, and
// the last knownSessionIDsRecentCap runs per job from runStore.
//
// The returned map is READ-ONLY and shared: callers MUST NOT mutate or persist
// it. The set is only ever replaced wholesale or dropped by
// invalidateKnownSessionsCache, so handing it out is race-free for read-only
// consumers. Returns an empty (non-nil) map on a nil Scheduler or no jobs.
func (s *Scheduler) KnownSessionIDs() map[string]struct{} {
	if s == nil {
		return map[string]struct{}{}
	}

	if set, ok := s.knownSessionsCache.lookupFresh(); ok {
		return set
	}

	// Snapshot the generation before reading source data so a concurrent
	// invalidate() during the build is not lost to our publish; the freshly
	// built set is still returned even when publish is rejected (#1811).
	buildGen := s.knownSessionsCache.beginBuild()
	set := s.buildKnownSessionsSet()
	s.knownSessionsCache.publish(set, buildGen)

	return set
}

// buildKnownSessionsSet does the actual O(jobs × recentCap) walk that
// KnownSessionIDs serves out of cache.
func (s *Scheduler) buildKnownSessionsSet() map[string]struct{} {
	jobIDsPtr := jobIDsScratchPool.Get().(*[]string)
	jobIDs := (*jobIDsPtr)[:0]

	// Allocate the map BEFORE taking the RLock so make() does not run inside
	// the lock window; a fixed initial capacity trades a few rehashes for a
	// shorter lock hold.
	out := make(map[string]struct{}, 32)
	s.mu.RLock()
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

	// Persisted history: RecentSessionIDs reads SessionID strings straight off
	// the cache ring instead of value-copying full []CronRunSummary rows
	// (~4 KB each). RunStore is nil only in tests.
	if s.runStoreEnabled() {
		for _, jobID := range jobIDs {
			for _, sid := range s.recentSessionIDs(jobID, knownSessionIDsRecentCap) {
				out[sid] = struct{}{}
			}
		}
	}

	// Put only after all reads are complete; drop oversize slices to avoid
	// pinning a burst-inflated backing array.
	if cap(jobIDs) <= jobIDsScratchCapDrop {
		// Clear string references to prevent pinning stale job ID strings.
		clear(jobIDs)
		*jobIDsPtr = jobIDs[:0]
		jobIDsScratchPool.Put(jobIDsPtr)
	}

	return out
}

// invalidateKnownSessionsCache clears the TTL snapshot so the next
// KnownSessionIDs call rebuilds. Called from mutator paths that can change
// the set: LastSessionID writes and runStore.Append. Cheap, so callers can
// invoke unconditionally.
func (s *Scheduler) invalidateKnownSessionsCache() {
	if s == nil {
		return
	}
	s.knownSessionsCache.invalidate()
}
