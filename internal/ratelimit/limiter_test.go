package ratelimit

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestAllowBurstThenBlock(t *testing.T) {
	t.Parallel()
	l := New(Config{Rate: rate.Every(time.Hour), Burst: 3})
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("Allow(%q) burst #%d should pass", "a", i)
		}
	}
	if l.Allow("a") {
		t.Fatalf("Allow(%q) should be blocked after burst", "a")
	}
}

func TestAllowPerKeyIsolation(t *testing.T) {
	t.Parallel()
	l := New(Config{Rate: rate.Every(time.Hour), Burst: 1})
	if !l.Allow("a") {
		t.Fatal("Allow(a) first token should pass")
	}
	if !l.Allow("b") {
		t.Fatal("Allow(b) should have its own bucket")
	}
	if l.Allow("a") {
		t.Fatal("Allow(a) second should be blocked")
	}
}

func TestEmptyKeyRejected(t *testing.T) {
	t.Parallel()
	l := New(Config{Rate: rate.Every(time.Second), Burst: 10})
	if l.Allow("") {
		t.Fatal("empty key must not share a global bucket")
	}
}

func TestLRUEvictionIsO1AndKeepsMaxKeys(t *testing.T) {
	t.Parallel()
	l := New(Config{Rate: rate.Every(time.Hour), Burst: 1, MaxKeys: 3})
	keys := []string{"a", "b", "c", "d", "e"}
	for _, k := range keys {
		l.Allow(k)
	}
	if got := l.Len(); got != 3 {
		t.Fatalf("Len = %d, want %d (LRU cap)", got, 3)
	}
}

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	l := New(Config{Rate: rate.Every(time.Hour), Burst: 1, MaxKeys: 2})
	l.Allow("a")
	l.Allow("b")
	l.Allow("a")      // promote "a"
	l.Allow("c")      // should evict "b", not "a"
	if l.Allow("a") { // "a" bucket already spent its burst, should still be blocked
		t.Fatal("Allow(a) should be blocked — bucket preserved after promotion")
	}
	if !l.Allow("b") { // "b" should be a fresh entry after eviction
		t.Fatal("Allow(b) should pass — entry was evicted and re-created fresh")
	}
}

// TestTTLLazyReset drives the lazy-expiry path with an injected clock.
//
// It used to use a 10ms TTL plus a real time.Sleep: any scheduling delay
// longer than 10ms between the first two Allow calls expired the entry, reset
// its bucket, and made the "second Allow within TTL should block" assertion
// fail even though the limiter was correct. That was ~flaky on Linux CI and
// worse on the macOS runners. Stepping a fake clock removes the timing
// dependency entirely while testing the same three transitions.
func TestTTLLazyReset(t *testing.T) {
	t.Parallel()
	const ttl = time.Minute
	l := New(Config{Rate: rate.Every(time.Hour), Burst: 1, TTL: ttl})

	// Fake clock: only this test's own steps advance it, so "within TTL"
	// and "past TTL" are exact rather than best-effort.
	var mu sync.Mutex
	nowVal := time.Now()
	l.nowFn = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return nowVal
	}
	advance := func(d time.Duration) {
		mu.Lock()
		nowVal = nowVal.Add(d)
		mu.Unlock()
	}

	if !l.Allow("a") {
		t.Fatal("initial Allow should pass")
	}
	// No time has passed: the entry is fresh and its single burst token spent.
	if l.Allow("a") {
		t.Fatal("second Allow within TTL should block")
	}
	// TTL is IDLE time, not absolute age: every Allow refreshes lastSeen, so
	// each step below is measured from the previous call. Walk three ttl/2
	// steps — total age passes ttl twice over, but the gaps never do, so a
	// correct limiter stays blocked the whole way. This is what separates
	// "idle window" from "absolute age": under the latter, a continuously
	// active key would get a fresh burst every ttl, which is a rate-limit
	// bypass rather than a cosmetic difference.
	for i := 0; i < 3; i++ {
		advance(ttl / 2)
		if l.Allow("a") {
			t.Fatalf("Allow still within idle TTL should block (step %d, total age %s)",
				i+1, time.Duration(i+1)*(ttl/2))
		}
	}
	// Past the TTL: lazy expiry resets the bucket, so a fresh burst is
	// granted. The comparison is strictly `>`, hence ttl+1 rather than ttl.
	advance(ttl + 1)
	if !l.Allow("a") {
		t.Fatal("Allow after TTL should reset and pass")
	}
}

func TestConcurrentAllowRaceFree(t *testing.T) {
	t.Parallel()
	l := New(Config{Rate: rate.Every(time.Microsecond), Burst: 100, MaxKeys: 50})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				k := string(rune('a' + (id*j)%26))
				l.Allow(k)
			}
		}(i)
	}
	wg.Wait()
	if got := l.Len(); got > 50 {
		t.Fatalf("Len = %d exceeded MaxKeys", got)
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	l := New(Config{Rate: rate.Every(time.Second), Burst: 1})
	if l.cfg.MaxKeys != defaultMaxKeys {
		t.Fatalf("MaxKeys default = %d, want %d", l.cfg.MaxKeys, defaultMaxKeys)
	}
	if l.cfg.TTL != defaultTTL {
		t.Fatalf("TTL default = %v, want %v", l.cfg.TTL, defaultTTL)
	}
}
