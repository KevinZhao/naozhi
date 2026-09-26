package server

import (
	"strconv"
	"testing"
	"time"
)

func newTestClientSubs() *clientSubs {
	return &clientSubs{unsubs: make(map[string]func()), gen: make(map[string]uint64)}
}

// markReleasable stands in for dropLocked's scheduling step.
func (cs *clientSubs) markReleasable(key string, nowNanos int64) {
	if cs.releaseAt == nil {
		cs.releaseAt = make(map[string]int64)
	}
	cs.releaseAt[key] = nowNanos + subGenRetentionNanos
}

// TestSubGenReclaim_MarkAndSweepBeyondRetention: a client flapping through many
// session subscriptions eventually reclaims their generations, so long-lived
// dashboard connections do not accumulate them indefinitely — but not inside
// the retention window, where a stale resubscribe loop may still be parked.
func TestSubGenReclaim_MarkAndSweepBeyondRetention(t *testing.T) {
	t.Parallel()

	cs := newTestClientSubs()
	t0 := time.Unix(1_000_000, 0).UnixNano()
	for _, key := range []string{"a", "b", "c"} {
		cs.gen[key] = 7
		cs.markReleasable(key, t0)
	}

	inWindow := t0 + int64(10*time.Second)
	if n := cs.sweepExpired(inWindow); n != 0 {
		t.Fatalf("sweep inside retention window reclaimed %d entries, want 0", n)
	}
	if len(cs.gen) != 3 {
		t.Fatalf("gen len after in-window sweep = %d, want 3 (stale-goroutine contract broken)", len(cs.gen))
	}

	// Past retention; reset the throttle the first sweep armed.
	cs.lastSweepNs = 0
	past := t0 + subGenRetentionNanos + int64(time.Second)
	if n := cs.sweepExpired(past); n != 3 {
		t.Fatalf("sweep past retention reclaimed %d entries, want 3", n)
	}
	if len(cs.gen) != 0 || len(cs.releaseAt) != 0 {
		t.Fatalf("after sweep gen=%d releaseAt=%d entries, want 0/0", len(cs.gen), len(cs.releaseAt))
	}
}

// TestSubGenReclaim_ActiveSubscriptionPreservesEntry: a marker that outlives a
// fresh subscribe must not take the live generation with it — losing gen[key]
// would collapse the counter and defeat takeover detection. Only the stale
// marker goes.
func TestSubGenReclaim_ActiveSubscriptionPreservesEntry(t *testing.T) {
	t.Parallel()

	cs := newTestClientSubs()
	cs.unsubs["active-key"] = func() {}
	cs.gen["active-key"] = 5
	t0 := time.Unix(2_000_000, 0).UnixNano()
	cs.markReleasable("active-key", t0)

	_ = cs.sweepExpired(t0 + subGenRetentionNanos + int64(time.Second))

	if gen, ok := cs.gen["active-key"]; !ok || gen != 5 {
		t.Fatalf("sweep deleted a live generation (got %v, ok=%v)", gen, ok)
	}
	if _, ok := cs.releaseAt["active-key"]; ok {
		t.Error("stale marker survived the sweep for a live key")
	}
}

// TestSubGenReclaim_SweepThrottle: two sweeps within
// subGenSweepMinIntervalNanos — the second is a no-op.
func TestSubGenReclaim_SweepThrottle(t *testing.T) {
	t.Parallel()

	cs := newTestClientSubs()
	cs.gen["x"] = 1
	t0 := time.Unix(4_000_000, 0).UnixNano()
	past := t0 + subGenRetentionNanos + int64(time.Second)

	cs.markReleasable("x", t0)
	if n := cs.sweepExpired(past); n != 1 {
		t.Fatalf("first sweep reclaimed %d, want 1", n)
	}

	cs.gen["y"] = 1
	cs.markReleasable("y", t0)
	soon := past + int64(time.Second) // within the 30s throttle
	if n := cs.sweepExpired(soon); n != 0 {
		t.Errorf("throttled sweep reclaimed %d, want 0", n)
	}
	if _, stillThere := cs.gen["y"]; !stillThere {
		t.Error("throttle did not prevent reclamation")
	}
}

// TestSubGenReclaim_HighWaterForcesSweep: past subGenHighWaterMark markers a
// sweep runs even inside the throttle, bounding memory on pathological
// clients.
func TestSubGenReclaim_HighWaterForcesSweep(t *testing.T) {
	t.Parallel()

	cs := newTestClientSubs()
	t0 := time.Unix(5_000_000, 0).UnixNano()
	for i := 0; i < subGenHighWaterMark+10; i++ {
		key := "k" + strconv.Itoa(i)
		cs.gen[key] = 1
		cs.markReleasable(key, t0)
	}
	past := t0 + subGenRetentionNanos + int64(time.Second)
	cs.lastSweepNs = past - int64(time.Second) // a sweep 1s ago: inside the throttle

	if reclaimed := cs.sweepExpired(past); reclaimed == 0 {
		t.Fatal("high-water sweep was suppressed — memory bound not enforced")
	}
	if len(cs.releaseAt) != 0 {
		t.Errorf("after high-water sweep, %d markers remain", len(cs.releaseAt))
	}
}

// TestSubGenReclaim_UnsubscribeSchedulesAndResubscribeCancels drives the
// registry the way the handlers do: unsubscribe keeps the generation but
// schedules it for reclamation; subscribing to the key again cancels that,
// and the generation keeps counting up rather than restarting.
func TestSubGenReclaim_UnsubscribeSchedulesAndResubscribeCancels(t *testing.T) {
	t.Parallel()

	r := newSubscriberRegistry()
	c := &wsClient{done: make(chan struct{})}
	r.add(c)
	admit := func() bool { return true }
	subscribe := func() uint64 {
		t.Helper()
		if res := r.reserve(c, "k"); res != reserveOK {
			t.Fatalf("reserve = %v", res)
		}
		gen, ok := r.install(c, "k", func() {}, admit)
		if !ok {
			t.Fatal("install declined")
		}
		return gen
	}
	cs := r.clients[c]

	if gen := subscribe(); gen != 1 {
		t.Fatalf("first generation = %d, want 1", gen)
	}
	t0 := time.Unix(6_000_000, 0).UnixNano()
	r.unsubscribe(c, "k", t0)
	if got, ok := cs.releaseAt["k"]; !ok || got != t0+subGenRetentionNanos {
		t.Fatalf("unsubscribe did not schedule reclamation (releaseAt=%v, ok=%v)", got, ok)
	}
	if cs.gen["k"] != 1 {
		t.Fatalf("unsubscribe dropped the generation (%d): a parked loop's gen=1 could match a fresh subscribe", cs.gen["k"])
	}

	if gen := subscribe(); gen != 2 {
		t.Errorf("resubscribe generation = %d, want 2", gen)
	}
	if _, marked := cs.releaseAt["k"]; marked {
		t.Error("resubscribe left the reclamation marker: a later sweep would delete the live generation")
	}

	// The next unsubscribe past the retention of an older marker sweeps it.
	cs.gen["old"] = 3
	cs.markReleasable("old", t0)
	r.unsubscribe(c, "k", t0+subGenRetentionNanos+int64(time.Second))
	if _, ok := cs.gen["old"]; ok {
		t.Error("unsubscribe did not sweep an expired generation")
	}
}
