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

// TestKnownSessionsCache_InvalidateBumpsGen pins R20260609-072532-LB-1
// (#1987): the gen counter MUST advance on EVERY invalidate(), whether it
// performs a real drop or only coalesces (within minInvalidateInterval). The
// #1811 strong invariant — "any invalidate() that lands after beginBuild()
// bumps gen, so publish() refuses the now-stale set" — would otherwise be
// broken by the PERF-3 coalesce branch (#1965), which only set dirty.
//
// Each case snapshots gen via beginBuild(), runs invalidate() under the named
// precondition, and asserts (a) gen advanced and (b) a build that started
// before the invalidate is rejected by publish().
func TestKnownSessionsCache_InvalidateBumpsGen(t *testing.T) {
	tests := []struct {
		name string
		// setup leaves the cache in the precondition that selects the
		// real-drop vs coalesce branch of invalidate().
		setup func(c *knownSessionsCache)
	}{
		{
			name: "cold cache real-drop branch (lastInvalidatedAt zero)",
			setup: func(c *knownSessionsCache) {
				// zero lastInvalidatedAt → real drop branch.
			},
		},
		{
			name: "past interval real-drop branch",
			setup: func(c *knownSessionsCache) {
				c.mu.Lock()
				c.lastInvalidatedAt = time.Now().Add(-minInvalidateInterval - time.Second)
				c.mu.Unlock()
			},
		},
		{
			name: "within interval coalesce branch",
			setup: func(c *knownSessionsCache) {
				// A recent real drop so the next invalidate coalesces.
				c.mu.Lock()
				c.lastInvalidatedAt = time.Now()
				c.set = map[string]struct{}{"live": {}}
				c.generatedAt = time.Now()
				c.mu.Unlock()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c knownSessionsCache
			tt.setup(&c)

			// A build that snapshots gen BEFORE the invalidate, then reads
			// (now-stale) source data, must lose the publish race.
			buildGen := c.beginBuild()
			c.invalidate()

			if c.gen == buildGen {
				t.Fatalf("invalidate() did not bump gen (%d == %d); #1811 invariant broken in %q branch (#1987)", c.gen, buildGen, tt.name)
			}
			if c.publish(map[string]struct{}{"stale-missing-X": {}}, buildGen) {
				t.Fatalf("publish() installed a set built before invalidate() in %q branch — coalesced invalidate failed to reject the lost build (#1987)", tt.name)
			}
		})
	}
}

// TestKnownSessionsCache_CoalescedInvalidateRejectsInFlightBuild reproduces the
// exact #1987 interleaving end-to-end: a cron run finishes mid-build and writes
// a new LastSessionID; the build that started earlier must not be able to
// publish a set that omits the new sessionID.
func TestKnownSessionsCache_CoalescedInvalidateRejectsInFlightBuild(t *testing.T) {
	var c knownSessionsCache

	// (1) A prior real drop, then publish a set so we are in steady state with
	// a recent lastInvalidatedAt (the coalesce window is open).
	c.invalidate() // real drop (cold)
	if !c.publish(map[string]struct{}{"old": {}}, c.beginBuild()) {
		t.Fatal("setup publish failed")
	}

	// (2) An in-flight dashboard build snapshots gen, then reads source data
	// (still missing the about-to-be-written sessionID X).
	buildGen := c.beginBuild()
	staleSet := map[string]struct{}{"old": {}} // no "X"

	// (3) A cron run finishes: appendRun writes LastSessionID=X, then calls
	// invalidate(). This is inside minInvalidateInterval → coalesce branch.
	c.invalidate()

	// (4) The in-flight build tries to publish its stale set. Pre-fix this
	// succeeded (gen unchanged) and clobbered the cache with a set missing X
	// for the full TTL. It must now be rejected.
	if c.publish(staleSet, buildGen) {
		t.Fatal("in-flight build published a set missing the just-written sessionID; coalesced invalidate did not bump gen (#1987)")
	}

	// (5) Because publish was rejected, the next lookupFresh rebuilds from
	// current source data rather than serving the stale set. The pending dirty
	// drop is honoured once the interval elapses.
	c.mu.Lock()
	c.lastInvalidatedAt = time.Now().Add(-minInvalidateInterval - time.Second)
	c.mu.Unlock()
	if set, ok := c.lookupFresh(); ok || set != nil {
		t.Fatalf("lookupFresh served a stale set after rejected publish: (%v, %v), want rebuild (nil,false)", set, ok)
	}
}
