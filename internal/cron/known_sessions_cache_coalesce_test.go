package cron

import (
	"testing"
	"time"
)

// TestKnownSessionsCache_InvalidateCoalesces pins R20260608133928-PERF-3
// (#1965): a burst of invalidate() calls (one per runStore.Append /
// LastSessionID write) must NOT drop the snapshot on every call. The first
// invalidate after a publish drops once; subsequent invalidates inside
// minInvalidateInterval only mark the cache dirty, so lookupFresh keeps
// serving the live set until the interval elapses. This bounds the
// O(jobs × recentCap) cold rebuild rate on append-heavy deployments.
func TestKnownSessionsCache_InvalidateCoalesces(t *testing.T) {
	var c knownSessionsCache
	seed := map[string]struct{}{"sess-a": {}}

	// First invalidate on a cold cache: lastInvalidatedAt is zero, so it
	// performs a real drop and stamps the timestamp.
	c.invalidate()

	// Publish a fresh set. publish clears dirty, so lookupFresh hits.
	if !c.publish(seed, c.beginBuild()) {
		t.Fatal("publish on quiescent cache = false, want true")
	}
	if _, ok := c.lookupFresh(); !ok {
		t.Fatal("lookupFresh after publish = miss, want hit")
	}

	// A second invalidate lands well inside minInvalidateInterval of the
	// first (real) drop. It must only mark dirty, leaving the set live.
	c.invalidate()
	if set, ok := c.lookupFresh(); !ok || set == nil {
		t.Fatal("coalesced invalidate dropped the set immediately; want still-fresh hit (#1965)")
	}

	// Many more invalidates in the same window: still a hit (coalesced).
	for i := 0; i < 100; i++ {
		c.invalidate()
	}
	if _, ok := c.lookupFresh(); !ok {
		t.Fatal("burst of invalidates dropped the cache; coalescing failed (#1965)")
	}

	// Once minInvalidateInterval elapses since the last real drop, the
	// pending dirty drop is honoured by lookupFresh and the cache misses.
	c.mu.Lock()
	c.lastInvalidatedAt = time.Now().Add(-minInvalidateInterval - time.Second)
	c.mu.Unlock()
	if set, ok := c.lookupFresh(); ok || set != nil {
		t.Fatalf("lookupFresh past coalescing window = (%v, %v), want (nil, false) — deferred drop not honoured", set, ok)
	}
}

// TestKnownSessionsCache_FirstInvalidateDropsImmediately ensures the
// coalescing change does not weaken correctness for an isolated invalidate:
// the very first invalidate after a publish (with a zeroed lastInvalidatedAt,
// i.e. no prior drop in the window) still drops synchronously so a single
// LastSessionID change is reflected without waiting for the interval.
func TestKnownSessionsCache_FirstInvalidateDropsImmediately(t *testing.T) {
	var c knownSessionsCache
	c.publish(map[string]struct{}{"x": {}}, c.beginBuild())
	if _, ok := c.lookupFresh(); !ok {
		t.Fatal("setup: lookupFresh after publish = miss")
	}

	// No prior drop has happened (lastInvalidatedAt zero) → real drop now.
	c.invalidate()
	if set, ok := c.lookupFresh(); ok || set != nil {
		t.Fatalf("first invalidate did not drop: (%v, %v), want (nil, false)", set, ok)
	}
}

// TestKnownSessionsCache_CoalescedInvalidateGuardsLostBuild verifies the gen
// guard still protects against a publish that raced a *real* (non-coalesced)
// invalidate, even with coalescing in place. A build that snapshots gen before
// a real drop must be rejected on publish.
func TestKnownSessionsCache_CoalescedInvalidateGuardsLostBuild(t *testing.T) {
	var c knownSessionsCache

	buildGen := c.beginBuild()
	// First invalidate (zero lastInvalidatedAt) → real drop bumps gen.
	c.invalidate()

	if c.publish(map[string]struct{}{"stale": {}}, buildGen) {
		t.Fatal("publish installed a set built before a real invalidate — gen guard broken")
	}
	if _, ok := c.lookupFresh(); ok {
		t.Fatal("lookupFresh after rejected publish = hit, want miss")
	}
}
