package server

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// seedSubscriberCounts populates both the source map and the lock-free
// subscriberCountFast mirror so hand-built test Hubs exercise the same
// singleSubscriber read path as production (which always maintains the
// mirror under h.mu). R20260531A-PERF-1 (#1522).
func seedSubscriberCounts(h *Hub, counts map[string]int) {
	h.subscriberCount = counts
	for k, n := range counts {
		var ctr atomic.Int32
		ctr.Store(int32(n))
		h.subscriberCountFast.Store(k, &ctr)
	}
}

// R249-PERF-30 (#944): pins the single-subscriber fast path that skips
// the marshal cache when only one tab is subscribed to a session key.
// The cache exists to coalesce N marshalPooled calls across N
// concurrent pushLoops on the same notify wave; for a single tab every
// notify advances lastTime so the fingerprint always misses and the
// cache slot allocation + per-key mutex round-trip is pure overhead.

func TestSingleSubscriberFastPath_BypassesCache(t *testing.T) {
	h := &Hub{
		mu:                  sync.RWMutex{},
		historyMarshalCache: newHistoryMarshalCache(),
	}
	seedSubscriberCounts(h, map[string]int{"only-tab": 1})
	entries := []cli.EventEntry{{Time: 1, Type: "user"}}
	if _, err := h.marshalHistoryFrame("only-tab", 0, entries); err != nil {
		t.Fatalf("marshalHistoryFrame: %v", err)
	}
	// Cache MUST stay cold: the fast path returns marshalPooled bytes
	// directly without touching historyMarshalCache. If a future
	// refactor accidentally re-routes through getOrMarshal the slot
	// would be populated and this assertion would catch the regression.
	if _, ok := h.historyMarshalCache.entries.Load("only-tab"); ok {
		t.Fatal("R249-PERF-30 regression: marshalHistoryFrame populated " +
			"historyMarshalCache slot for a single-subscriber key — fast path is " +
			"supposed to skip the cache entirely so the per-key mutex round-trip " +
			"is avoided for the lone tab.")
	}
}

func TestSingleSubscriberFastPath_MultiSubStillUsesCache(t *testing.T) {
	// With 2+ subscribers the cache MUST still be consulted so multi-tab
	// fan-out keeps coalescing the marshal call. Otherwise the R214-PERF-4
	// optimisation (which #944 explicitly preserves) would silently
	// regress.
	h := &Hub{
		mu:                  sync.RWMutex{},
		historyMarshalCache: newHistoryMarshalCache(),
	}
	seedSubscriberCounts(h, map[string]int{"two-tabs": 2})
	entries := []cli.EventEntry{{Time: 1, Type: "user"}}
	if _, err := h.marshalHistoryFrame("two-tabs", 0, entries); err != nil {
		t.Fatalf("marshalHistoryFrame: %v", err)
	}
	if _, ok := h.historyMarshalCache.entries.Load("two-tabs"); !ok {
		t.Fatal("R249-PERF-30 wiring regression: marshalHistoryFrame " +
			"failed to populate historyMarshalCache for a 2-subscriber key " +
			"— multi-tab fan-out lost its coalescing fast path (R214-PERF-4).")
	}
}

func TestSingleSubscriber_ReportsCorrectCount(t *testing.T) {
	tests := []struct {
		name  string
		count int
		want  bool
	}{
		{"zero", 0, false},
		{"one", 1, true},
		{"two", 2, false},
		{"many", 50, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Hub{
				mu: sync.RWMutex{},
			}
			seedSubscriberCounts(h, map[string]int{"k": tc.count})
			if got := h.singleSubscriber("k"); got != tc.want {
				t.Fatalf("singleSubscriber(count=%d) = %v; want %v",
					tc.count, got, tc.want)
			}
		})
	}
}

// TestSubscriberCountFast_MirrorsWritePaths pins R20260531A-PERF-1
// (#1522): the lock-free subscriberCountFast mirror read by
// singleSubscriber must track the h.mu-guarded source map across the
// bump (setSubscriberCountFast) and decrement (decSubscriberCountLocked)
// write paths, including the delete-on-zero edge.
func TestSubscriberCountFast_MirrorsWritePaths(t *testing.T) {
	h := &Hub{
		mu:              sync.RWMutex{},
		subscriberCount: map[string]int{},
		enforceCaps:     true,
	}
	fast := func(key string) (int32, bool) {
		v, ok := h.subscriberCountFast.Load(key)
		if !ok {
			return 0, false
		}
		return v.(*atomic.Int32).Load(), true
	}

	// Two bumps → count 2, mirror 2, singleSubscriber false.
	h.mu.Lock()
	h.subscriberCount["k"]++
	h.setSubscriberCountFast("k", h.subscriberCount["k"])
	h.subscriberCount["k"]++
	h.setSubscriberCountFast("k", h.subscriberCount["k"])
	h.mu.Unlock()
	if n, ok := fast("k"); !ok || n != 2 {
		t.Fatalf("after 2 bumps: fast mirror = (%d,%v); want (2,true)", n, ok)
	}
	if h.singleSubscriber("k") {
		t.Fatal("singleSubscriber must be false at count 2")
	}

	// One decrement → count 1, singleSubscriber true.
	h.mu.Lock()
	h.decSubscriberCountLocked("k")
	h.mu.Unlock()
	if n, ok := fast("k"); !ok || n != 1 {
		t.Fatalf("after 1 dec: fast mirror = (%d,%v); want (1,true)", n, ok)
	}
	if !h.singleSubscriber("k") {
		t.Fatal("singleSubscriber must be true at count 1")
	}

	// Final decrement → entry deleted from BOTH maps.
	h.mu.Lock()
	h.decSubscriberCountLocked("k")
	h.mu.Unlock()
	if _, ok := h.subscriberCount["k"]; ok {
		t.Fatal("source map entry must be deleted at count 0")
	}
	if _, ok := fast("k"); ok {
		t.Fatal("fast mirror entry must be deleted at count 0 — a leaked " +
			"*atomic.Int32 would let singleSubscriber read a stale count")
	}
	if h.singleSubscriber("k") {
		t.Fatal("singleSubscriber must be false after the key is fully torn down")
	}
}

func TestSingleSubscriber_NilCounterFallsThroughToCache(t *testing.T) {
	// Test fixtures that build a Hub without subscriberCount must
	// continue to use the cached path — the fast-path gate is strictly
	// additive, never bypasses the legacy behaviour, and never panics
	// on the nil map. R040034-style hand-built Hubs in older tests
	// still rely on this contract.
	h := &Hub{}
	if got := h.singleSubscriber("any"); got {
		t.Fatal("nil subscriberCount must report singleSubscriber=false " +
			"(force the legacy cached path); reporting true would panic on the " +
			"nil map read.")
	}
}
