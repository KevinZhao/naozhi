package ratelimit

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestAllowBurstThenBlock(t *testing.T) {
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
	l := New(Config{Rate: rate.Every(time.Second), Burst: 10})
	if l.Allow("") {
		t.Fatal("empty key must not share a global bucket")
	}
}

func TestLRUEvictionIsO1AndKeepsMaxKeys(t *testing.T) {
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

func TestTTLLazyReset(t *testing.T) {
	l := New(Config{Rate: rate.Every(time.Hour), Burst: 1, TTL: 10 * time.Millisecond})
	if !l.Allow("a") {
		t.Fatal("initial Allow should pass")
	}
	if l.Allow("a") {
		t.Fatal("second Allow within TTL should block")
	}
	time.Sleep(20 * time.Millisecond)
	if !l.Allow("a") {
		t.Fatal("Allow after TTL should reset and pass")
	}
}

func TestConcurrentAllowRaceFree(t *testing.T) {
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
	l := New(Config{Rate: rate.Every(time.Second), Burst: 1})
	if l.cfg.MaxKeys != defaultMaxKeys {
		t.Fatalf("MaxKeys default = %d, want %d", l.cfg.MaxKeys, defaultMaxKeys)
	}
	if l.cfg.TTL != defaultTTL {
		t.Fatalf("TTL default = %v, want %v", l.cfg.TTL, defaultTTL)
	}
}
