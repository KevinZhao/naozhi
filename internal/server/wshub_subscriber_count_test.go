package server

import (
	"testing"
)

// TestSubscriberCount_DecHelper covers the decrement helper introduced for
// R246-PERF-4 (#716). The map invariant is "key absent ⇔ count==0" so the
// dashboard's per-key cap check stays O(1) and the map size stays bounded
// by the number of keys that currently have at least one subscriber.
//
// Pure helper test — no Hub state machine, no goroutines. The full lifecycle
// (subscribe → unsubscribe / shutdown / unregister increments + decrements
// the counter end-to-end) is exercised by the existing TestHub_Subscribe_*
// tests and by the per-key cap test that pre-seeds the counter directly.
func TestSubscriberCount_DecHelper(t *testing.T) {
	h := &Hub{
		subscriberCount: map[string]int{
			"k1": 3,
			"k2": 1,
		},
	}

	// Decrement above 1 -> stays in map.
	h.decSubscriberCountLocked("k1")
	if got := h.subscriberCount["k1"]; got != 2 {
		t.Errorf("k1 after dec: got %d, want 2", got)
	}
	if _, ok := h.subscriberCount["k1"]; !ok {
		t.Error("k1 should still be present after first decrement")
	}

	// Decrement at 1 -> removed from map.
	h.decSubscriberCountLocked("k2")
	if _, ok := h.subscriberCount["k2"]; ok {
		t.Errorf("k2 should be removed when count hits 0; got %v", h.subscriberCount)
	}

	// Decrement on missing key -> defensive no-op (helper must not panic
	// or insert a negative count). A future refactor that removes a
	// no-longer-tracked key from c.subscriptions would otherwise corrupt
	// the counter.
	h.decSubscriberCountLocked("missing")
	if _, ok := h.subscriberCount["missing"]; ok {
		t.Errorf("missing key should not be inserted; got %v", h.subscriberCount)
	}

	// Nil-map Hub (older test harness skipping NewHub) -> no-op + no panic.
	hNil := &Hub{}
	hNil.decSubscriberCountLocked("anything")
}
