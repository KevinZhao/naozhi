// scheduler_session_cache.go: TTL-bounded KnownSessionIDs snapshot cache.

package cron

import (
	"sync"
	"time"
)

// knownSessionsCache holds a TTL-bounded snapshot of KnownSessionIDs
// output. set is read-only after publication so callers can hand out
// the map directly without copying.
type knownSessionsCache struct {
	mu          sync.RWMutex
	generatedAt time.Time
	set         map[string]struct{}
	// gen is bumped on every invalidate so a concurrent build that started
	// before it cannot publish a stale set: callers snapshot gen via
	// beginBuild() before reading source data and pass it to publish(), which
	// installs only if gen is unchanged (#1811).
	gen uint64
	// lastInvalidatedAt/dirty coalesce invalidate(): the set is dropped at most
	// once per minInvalidateInterval; dirty marks a pending drop that lookupFresh
	// honours once the interval elapses (#1965).
	lastInvalidatedAt time.Time
	dirty             bool
}

// lookupFresh returns the cached set when it is populated and still within
// knownSessionsCacheTTL of generatedAt. ok is false on a cold or expired
// cache. The returned map is the shared read-only snapshot (never mutated in
// place — publish replaces it wholesale), so callers may hand it out directly.
func (c *knownSessionsCache) lookupFresh() (map[string]struct{}, bool) {
	c.mu.RLock()
	// Cold cache: skip the time.Now() read on this hot path (containsSessionID
	// and the 1Hz dashboard poll both land here on every call).
	if c.set == nil {
		c.mu.RUnlock()
		return nil, false
	}
	now := time.Now()
	if now.Sub(c.generatedAt) < knownSessionsCacheTTL {
		// A coalesced invalidate (dirty) is honoured only once
		// minInvalidateInterval has elapsed since the last real drop, so a
		// burst of Appends does not force a rebuild on every probe.
		if !c.dirty || now.Sub(c.lastInvalidatedAt) < minInvalidateInterval {
			set := c.set
			c.mu.RUnlock()
			return set, true
		}
	}
	c.mu.RUnlock()

	// The set is stale (TTL expired) or a coalesced invalidate is now due.
	// Promote the pending dirty drop under the write lock so the next caller
	// rebuilds and the dirty flag does not linger.
	if set, ok := c.lookupFreshFlush(); ok {
		return set, ok
	}
	return nil, false
}

// lookupFreshFlush re-checks freshness under the write lock and, when a
// coalesced invalidate is now due (dirty + past minInvalidateInterval),
// performs the deferred drop. Split out so the common read path in
// lookupFresh stays on the RLock. Returns the still-fresh set when the
// deferred drop was NOT triggered (a concurrent publish may have refreshed it).
func (c *knownSessionsCache) lookupFreshFlush() (map[string]struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.set == nil || now.Sub(c.generatedAt) >= knownSessionsCacheTTL {
		return nil, false
	}
	if c.dirty && now.Sub(c.lastInvalidatedAt) >= minInvalidateInterval {
		c.set = nil
		c.dirty = false
		c.lastInvalidatedAt = now
		c.gen++
		return nil, false
	}
	return c.set, true
}

// beginBuild snapshots the current generation counter. A caller that is
// about to build a fresh set MUST call this BEFORE reading any source data
// (Job.LastSessionID, runStore, …) and pass the returned token to publish().
// Any invalidate() that lands after beginBuild() bumps gen, so publish()
// will refuse to install the now-stale set (#1811).
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
// is stale and must be discarded).
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
	// A fresh build reflects the latest source data, so any pending coalesced
	// invalidate is satisfied: clear dirty so lookupFresh serves this set for
	// the full TTL rather than re-dropping it on the next probe.
	c.dirty = false
	return true
}

// invalidate marks the snapshot stale, coalescing bursts: it performs the
// actual drop (nil set + gen bump) at most once per minInvalidateInterval and
// otherwise sets `dirty` so lookupFresh performs the deferred drop once the
// interval elapses (#1965). Cheap (one mutex) so mutator paths call it
// unconditionally.
func (c *knownSessionsCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastInvalidatedAt.IsZero() || time.Since(c.lastInvalidatedAt) >= minInvalidateInterval {
		c.set = nil
		c.gen++
		c.dirty = false
		c.lastInvalidatedAt = time.Now()
		return
	}
	// Within the coalescing window: defer the set drop but STILL bump gen so an
	// in-flight build that called beginBuild() before this invalidate has its
	// publish() rejected — otherwise it could publish a stale set AND clear
	// dirty, serving stale data for the full TTL (#1987).
	c.gen++
	c.dirty = true
}

// knownSessionsCacheTTL bounds how stale a cached KnownSessionIDs snapshot may
// be: well below the auto-workspace-chain spawn cadence (one spawn per user
// message); dashboard 1Hz pollers see at most one rebuild per cache cycle.
const knownSessionsCacheTTL = 30 * time.Second

// minInvalidateInterval bounds how often invalidate() actually drops the
// snapshot: runStore.Append and LastSessionID writes call it many times per
// second, and without coalescing every spawn-time probe paid the
// O(jobs × recentCap) cold rebuild (#1965).
const minInvalidateInterval = 5 * time.Second
