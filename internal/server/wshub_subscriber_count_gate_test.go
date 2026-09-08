package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestDropMarshalCacheForLocked drives the real predicate that decides whether a
// historyMarshalCache slot is released when a subscriber leaves.
//
// It replaced a table test over a MIRRORED copy of the expression plus a
// source-shape pin asserting the production string byte-for-byte (#2623). Two
// drift sources for one decision: the mirror could disagree with production, and
// the pin could only tell you the string changed, not whether the behaviour was
// still right. dropMarshalCacheForLocked is now a method, so the test drives the
// thing that runs and both guards are unnecessary.
//
// The predicate also lost its enforceCaps term. R040034-CHANGES had made
// `!enforceCaps || count == 0` load-bearing for one shape only: a hand-rolled
// &Hub{subscriberCount: make(...)} with the flag left false, which got an
// unconditional drop. That shape no longer exists — since #2623 allocating the
// map IS what activates the counter — and for every hub the flag actually
// described (built without NewHub, nil map) a nil map already reads 0, so the
// remaining term answers true on its own. Production, where NewHub always set
// the flag, is unaffected.
func TestDropMarshalCacheForLocked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		count map[string]int
		want  bool
	}{
		{"nil map (hand-rolled hub, no counter) → drop", nil, true},
		{"key absent → drop (last subscriber gone)", map[string]int{"other": 3}, true},
		{"count 0 → drop", map[string]int{"k": 0}, true},
		{"count 1 → keep (one other subscriber still needs the slot)", map[string]int{"k": 1}, false},
		{"count 5 → keep", map[string]int{"k": 5}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Hub{subscriberCount: tc.count}
			if got := h.dropMarshalCacheForLocked("k"); got != tc.want {
				t.Errorf("dropMarshalCacheForLocked(k) with %v = %v, want %v", tc.count, got, tc.want)
			}
		})
	}
}

// TestSubscriberCount_NilMapNeverPanics pins the one place enforceCaps was NOT
// redundant. handleSubscribe increments h.subscriberCount[key], and writing to a
// nil map panics — the flag used to keep hand-rolled hubs out of that branch, and
// the allocation check does it now. A regression here is a panic on the first
// subscribe of any Hub built without NewHub.
func TestSubscriberCount_NilMapNeverPanics(t *testing.T) {
	t.Parallel()
	h := &Hub{} // no subscriberCount
	// Reads must be safe...
	if !h.dropMarshalCacheForLocked("k") {
		t.Error("nil-map hub: dropMarshalCacheForLocked should report drop")
	}
	h.decSubscriberCountLocked("k")
	// ...and the cap comparison must be false rather than a panic, so a
	// nil-counter hub is never refused with "too many subscribers for key".
	if h.subscriberCount["k"] >= maxSubscribersPerKey {
		t.Errorf("nil-map hub reads %d for an absent key, want 0 (< %d)", h.subscriberCount["k"], maxSubscribersPerKey)
	}
}

// TestHandleSubscribe_NilCounterMapDoesNotPanic covers the branch the guard
// exists for, which nothing covered before.
//
// Removing `h.subscriberCount != nil` from handleSubscribe and running the whole
// Subscribe/SubscriberCount suite produced no failure: every hub that reaches
// handleSubscribe in the tests comes from NewHub, so the nil-map write was
// unreachable and the guard was protecting a path no test took. A guard nothing
// exercises is a guard that can be deleted by accident, so this drives it.
//
// A hand-rolled Hub with a nil counter must serve a subscribe as a no-op on the
// counter rather than panicking on a nil-map write. The router is nil too, so the
// request stops at "session not found" — which is fine: the assertion is that the
// counter branch is reached and survives, and the placeholder reservation it
// installs is cleaned up.
func TestHandleSubscribe_NilCounterMapDoesNotPanic(t *testing.T) {
	t.Parallel()
	// A real but empty router, so the request reaches the counter branch instead
	// of dying earlier on a nil router — which is what the first version of this
	// test did, and why it proved nothing about the guard.
	h := &Hub{router: session.NewRouter(session.RouterConfig{})} // no subscriberCount
	c := &wsClient{
		hub:           h,
		send:          make(chan []byte, 8),
		subscriptions: map[string]func(){},
		subGen:        map[string]uint64{},
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleSubscribe panicked on a nil subscriberCount: %v — the map's "+
				"existence is what activates the counter since #2623, and the write must stay guarded", r)
		}
	}()
	h.handleSubscribe(c, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})

	// The reservation must not be left behind: handleSubscribe installs a
	// placeholder before the session lookup and clears it on the not-found path.
	h.mu.Lock()
	left := len(c.subscriptions)
	h.mu.Unlock()
	if left != 0 {
		t.Errorf("subscriptions left behind after the not-found path: %d", left)
	}
}
