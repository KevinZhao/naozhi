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
	// lastInvalidatedAt records when the snapshot was last actually dropped.
	// R20260608133928-PERF-3 (#1965): invalidate() is called on *every*
	// runStore.Append and LastSessionID write. On an active deployment that
	// fires far more often than once per knownSessionsCacheTTL, so the TTL
	// snapshot almost never survived and containsSessionID paid the
	// O(jobs × recentCap) cold rebuild on nearly every spawn-time probe.
	// invalidate() now coalesces: it drops the set at most once per
	// minInvalidateInterval, setting `dirty` in between so the next eligible
	// call (or lookupFresh past the interval) still rebuilds. This bounds the
	// cold-rebuild rate to ~1 per minInvalidateInterval while keeping staleness
	// far below the 30s TTL.
	lastInvalidatedAt time.Time
	dirty             bool
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
	if c.set != nil && time.Since(c.generatedAt) < knownSessionsCacheTTL {
		// A coalesced invalidate (dirty) is honoured only once
		// minInvalidateInterval has elapsed since the last real drop, so a
		// burst of Appends does not force a rebuild on every probe.
		// R20260608133928-PERF-3 (#1965).
		if !c.dirty || time.Since(c.lastInvalidatedAt) < minInvalidateInterval {
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
	if c.set == nil || time.Since(c.generatedAt) >= knownSessionsCacheTTL {
		return nil, false
	}
	if c.dirty && time.Since(c.lastInvalidatedAt) >= minInvalidateInterval {
		c.set = nil
		c.dirty = false
		c.lastInvalidatedAt = time.Now()
		c.gen++
		return nil, false
	}
	return c.set, true
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
	// A fresh build reflects the latest source data, so any pending coalesced
	// invalidate is satisfied: clear dirty so lookupFresh serves this set for
	// the full TTL rather than re-dropping it on the next probe.
	c.dirty = false
	return true
}

// invalidate marks the snapshot stale, coalescing bursts: it performs the
// actual drop (nil set + gen bump) at most once per minInvalidateInterval. In
// between, it records a pending `dirty` flag so the deferred drop is honoured
// by lookupFresh once the interval elapses. This bounds the cold-rebuild rate
// on append-heavy deployments while keeping staleness well below the TTL.
// R20260608133928-PERF-3 (#1965). Cheap (one mutex) so mutator paths call it
// unconditionally.
func (c *knownSessionsCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastInvalidatedAt.IsZero() || time.Since(c.lastInvalidatedAt) >= minInvalidateInterval {
		// Far enough since the last real drop: drop now.
		c.set = nil
		c.gen++
		c.dirty = false
		c.lastInvalidatedAt = time.Now()
		return
	}
	// Within the coalescing window: defer the drop. The set stays live (and
	// gen-protected) so concurrent publishers and lookupFresh callers keep
	// serving it until lookupFresh flushes the dirty drop past the interval.
	c.dirty = true
}

// knownSessionsCacheTTL bounds how stale a cached KnownSessionIDs
// snapshot may be. 30s matches the godoc claim and is well below the
// auto-workspace-chain spawn cadence (one spawn per user message);
// dashboard 1Hz pollers see at most one rebuild per cache cycle. R250-PERF-7.
const knownSessionsCacheTTL = 30 * time.Second

// minInvalidateInterval bounds how often invalidate() actually drops the
// KnownSessionIDs snapshot. On an active deployment runStore.Append and
// LastSessionID writes call invalidate() many times per second; without
// coalescing the 30s TTL never survived and every spawn-time probe paid the
// O(jobs × recentCap) cold rebuild. Coalescing the drop to at most once per
// 5s caps the rebuild rate while keeping staleness an order of magnitude
// below the TTL. R20260608133928-PERF-3 (#1965).
const minInvalidateInterval = 5 * time.Second
