package cron

import (
	"testing"
	"time"
)

// TestKnownSessionsCache_LookupPublishInvalidate pins the TTL gate +
// publish/invalidate behaviour after R249-CR-4 / R260528-ARCH-7 (#948 / #1368)
// folded the open-coded lock/check/store triples into methods on the cache
// type. Cold lookup misses; publish makes a fresh snapshot hit; an aged
// snapshot misses again; invalidate forces a miss.
func TestKnownSessionsCache_LookupPublishInvalidate(t *testing.T) {
	var c knownSessionsCache

	// Cold cache: miss.
	if set, ok := c.lookupFresh(); ok || set != nil {
		t.Fatalf("cold lookupFresh = (%v, %v), want (nil, false)", set, ok)
	}

	// Publish then immediately read back the same shared snapshot.
	want := map[string]struct{}{"sess-a": {}, "sess-b": {}}
	c.publish(want)
	got, ok := c.lookupFresh()
	if !ok {
		t.Fatal("fresh lookupFresh after publish = (_, false), want hit")
	}
	if len(got) != len(want) {
		t.Fatalf("lookupFresh set len = %d, want %d", len(got), len(want))
	}
	if _, has := got["sess-a"]; !has {
		t.Fatal("published set missing sess-a")
	}

	// Age the snapshot past the TTL: lookupFresh must miss.
	c.mu.Lock()
	c.generatedAt = time.Now().Add(-knownSessionsCacheTTL - time.Second)
	c.mu.Unlock()
	if _, ok := c.lookupFresh(); ok {
		t.Fatal("expired lookupFresh = (_, true), want miss")
	}

	// Re-publish then invalidate: lookupFresh must miss again.
	c.publish(want)
	if _, ok := c.lookupFresh(); !ok {
		t.Fatal("lookupFresh after re-publish = miss, want hit")
	}
	c.invalidate()
	if set, ok := c.lookupFresh(); ok || set != nil {
		t.Fatalf("lookupFresh after invalidate = (%v, %v), want (nil, false)", set, ok)
	}
}
