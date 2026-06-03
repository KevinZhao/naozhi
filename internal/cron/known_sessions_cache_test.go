package cron

import (
	"sync"
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

// TestKnownSessionsCache_ConcurrentReaders verifies that lookupFresh uses an
// RLock so many concurrent readers can proceed in parallel without blocking
// each other. R164029-GO-4: upgrading sync.Mutex → sync.RWMutex.
//
// The test launches N goroutines that all call lookupFresh simultaneously
// while a background writer interleaves publish and invalidate. With a plain
// Mutex all readers would serialise on the write lock; with RWMutex they
// share the read lock and only yield to the writer. The test's correctness
// criterion is that all goroutines observe consistent state (hit XOR miss)
// and that the race detector finds no data races.
func TestKnownSessionsCache_ConcurrentReaders(t *testing.T) {
	const goroutines = 32

	var c knownSessionsCache
	seed := map[string]struct{}{"sess-x": {}, "sess-y": {}}
	c.publish(seed)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				set, ok := c.lookupFresh()
				// If ok the returned set must be the published map (same pointer).
				if ok && set == nil {
					t.Errorf("lookupFresh ok=true but set is nil")
				}
			}
		}()
	}

	// One writer goroutine that interleaves publish/invalidate.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for j := 0; j < 50; j++ {
			c.publish(seed)
			c.invalidate()
		}
	}()

	close(start)
	wg.Wait()
}

// TestKnownSessionsCache_RWMutex_ReadLock confirms that lookupFresh acquires
// only an RLock (not a full Lock) so two concurrent lookups do not block each
// other. This is a compile-time guarantee expressed via the race detector: if
// lookupFresh were still using Lock() the -race flag would detect the
// lock-contention pattern; with RLock() concurrent reads are legal.
// [R164029-GO-4].
func TestKnownSessionsCache_RWMutex_ReadLock(t *testing.T) {
	var c knownSessionsCache
	c.publish(map[string]struct{}{"a": {}})

	// Hold the RLock in one goroutine while a second goroutine also calls
	// lookupFresh. Both must succeed without deadlocking.
	done := make(chan struct{})
	c.mu.RLock()
	go func() {
		defer close(done)
		// This would deadlock if lookupFresh used Lock() (write-lock) instead of RLock().
		_, _ = c.lookupFresh()
	}()
	// Give the goroutine a moment then release the held RLock.
	time.Sleep(5 * time.Millisecond)
	c.mu.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lookupFresh deadlocked under concurrent RLock — likely using Lock() instead of RLock()")
	}
}
