// scheduler_session_cache.go: TTL-bounded KnownSessionIDs snapshot cache.
//
// Move-only split out of scheduler.go, co-located with its only callers in
// scheduler_session.go. No behaviour change — the type, its 4 methods, and
// the TTL const are pasted verbatim.

package cron

import (
	"sync"
	"time"
)

// knownSessionsCache holds a TTL-bounded snapshot of KnownSessionIDs
// output. Set is read-only after publication so callers can hand out
// the map directly without copying. R250-PERF-7.
type knownSessionsCache struct {
	mu          sync.RWMutex
	generatedAt time.Time
	set         map[string]struct{}
	// gen is bumped on every invalidate so a concurrent build that started
	// before the invalidate cannot publish its now-stale set. Callers
	// snapshot gen via beginBuild() before reading the source data and pass
	// it back to publish(); publish only installs the set if gen is
	// unchanged. R20260605B-CORR-7 (#1811).
	gen uint64
}

// lookupFresh returns the cached set when it is populated and still within
// knownSessionsCacheTTL of generatedAt. ok is false on a cold or expired
// cache. The returned map is the shared read-only snapshot (never mutated in
// place — publish replaces it wholesale), so callers may hand it out directly.
//
// R249-CR-4 / R260528-ARCH-7 (#948 / #1368): the lock + TTL-check + read
// triple was open-coded at containsSessionID + KnownSessionIDs; folding it
// into a method on the cache type keeps the TTL gate in one place and lets the
// cache own its own mutex instead of exposing c.mu to every Scheduler caller.
func (c *knownSessionsCache) lookupFresh() (map[string]struct{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.set != nil && time.Since(c.generatedAt) < knownSessionsCacheTTL {
		return c.set, true
	}
	return nil, false
}

// beginBuild snapshots the current generation counter. A caller that is
// about to build a fresh set MUST call this BEFORE reading any source data
// (Job.LastSessionID, runStore, …) and pass the returned token to publish().
// Any invalidate() that lands after beginBuild() bumps gen, so publish()
// will refuse to install the now-stale set. R20260605B-CORR-7 (#1811).
func (c *knownSessionsCache) beginBuild() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gen
}

// publish installs a freshly built set as the current snapshot and stamps
// generatedAt to now, but ONLY if no invalidate() landed since beginBuild()
// returned buildGen. The set MUST NOT be mutated after publication — readers
// from lookupFresh share it without copying. Returns true when the set was
// installed, false when a concurrent invalidate raced ahead (the caller's set
// is stale and must be discarded). R20260605B-CORR-7 (#1811).
func (c *knownSessionsCache) publish(set map[string]struct{}, buildGen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != buildGen {
		// An invalidate() ran between beginBuild() and here; the set was
		// built from source data older than that invalidate. Drop it so the
		// next lookupFresh misses and rebuilds from current data.
		return false
	}
	c.set = set
	c.generatedAt = time.Now()
	return true
}

// invalidate drops the snapshot so the next lookupFresh misses and forces a
// rebuild, and bumps gen so any in-flight build started before this call
// cannot publish over it. Cheap (one mutex + nil assign + increment) so
// mutator paths call it unconditionally.
func (c *knownSessionsCache) invalidate() {
	c.mu.Lock()
	c.set = nil
	c.gen++
	c.mu.Unlock()
}

// knownSessionsCacheTTL bounds how stale a cached KnownSessionIDs
// snapshot may be. 30s matches the godoc claim and is well below the
// auto-workspace-chain spawn cadence (one spawn per user message);
// dashboard 1Hz pollers see at most one rebuild per cache cycle. R250-PERF-7.
const knownSessionsCacheTTL = 30 * time.Second
