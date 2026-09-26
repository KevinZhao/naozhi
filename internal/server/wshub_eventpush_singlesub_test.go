package server

import (
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// hubWithSubscribers builds a bare Hub whose key has n subscribers.
func hubWithSubscribers(key string, n int) *Hub {
	h := &Hub{subs: newSubscriberRegistry(), historyMarshalCache: newHistoryMarshalCache()}
	for i := 0; i < n; i++ {
		registerSub(h, &wsClient{done: make(chan struct{})}, key)
	}
	return h
}

// R249-PERF-30 (#944): pins the single-subscriber fast path that skips
// the marshal cache when only one tab is subscribed to a session key.
// The cache exists to coalesce N marshalPooled calls across N
// concurrent pushLoops on the same notify wave; for a single tab every
// notify advances lastTime so the fingerprint always misses and the
// cache slot allocation + per-key mutex round-trip is pure overhead.

func TestSingleSubscriberFastPath_BypassesCache(t *testing.T) {
	h := hubWithSubscribers("only-tab", 1)
	entries := []clievent.EventEntry{{Time: 1, Type: "user"}}
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
	h := hubWithSubscribers("two-tabs", 2)
	entries := []clievent.EventEntry{{Time: 1, Type: "user"}}
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
		{"many", maxSubscribersPerKey, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := hubWithSubscribers("k", tc.count)
			if got := h.singleSubscriber("k"); got != tc.want {
				t.Fatalf("singleSubscriber(count=%d) = %v; want %v",
					tc.count, got, tc.want)
			}
		})
	}
}

// TestSubscriberCountFast_FollowsSubscribeAndUnsubscribe: the lock-free
// count singleSubscriber reads tracks the registry across subscribe,
// unsubscribe and unregister, and goes away with the last subscriber.
func TestSubscriberCountFast_FollowsSubscribeAndUnsubscribe(t *testing.T) {
	h := &Hub{subs: newSubscriberRegistry()}
	fast := func(key string) (int32, bool) {
		v, ok := h.subs.countFast.Load(key)
		if !ok {
			return 0, false
		}
		return v.(*atomic.Int32).Load(), true
	}
	a := &wsClient{done: make(chan struct{})}
	b := &wsClient{done: make(chan struct{})}
	registerSub(h, a, "k")
	registerSub(h, b, "k")
	if n, ok := fast("k"); !ok || n != 2 {
		t.Fatalf("two subscribers: fast count = (%d,%v); want (2,true)", n, ok)
	}
	if h.singleSubscriber("k") {
		t.Fatal("singleSubscriber must be false at count 2")
	}

	h.subs.unsubscribe(a, "k", 0)
	if n, ok := fast("k"); !ok || n != 1 {
		t.Fatalf("after one unsubscribe: fast count = (%d,%v); want (1,true)", n, ok)
	}
	if !h.singleSubscriber("k") {
		t.Fatal("singleSubscriber must be true at count 1")
	}

	h.subs.remove(b)
	if _, ok := fast("k"); ok {
		t.Fatal("fast count must be deleted with the last subscriber — a " +
			"leaked counter would let singleSubscriber read a stale value")
	}
	if subscriberCountOf(h, "k") != 0 || h.singleSubscriber("k") {
		t.Fatal("key still has subscribers after the last one left")
	}
}
