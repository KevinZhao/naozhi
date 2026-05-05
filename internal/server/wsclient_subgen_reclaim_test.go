package server

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// TestSubGenReclaim_MarkAndSweepBeyondRetention pins the core reclamation
// behaviour introduced by R175-P2. A wsClient that flaps through many
// session subscriptions must eventually reclaim subGen entries so long-lived
// dashboard connections do not accumulate the map indefinitely.
//
// Scenario: mark three keys for release at t0, advance past the retention
// window, trigger a sweep. All three subGen entries must be gone.
func TestSubGenReclaim_MarkAndSweepBeyondRetention(t *testing.T) {
	t.Parallel()

	c := &wsClient{
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	// Arrange: three historical subscribe/unsubscribe cycles.
	for _, key := range []string{"a", "b", "c"} {
		c.subGen[key] = 7
	}
	t0 := time.Unix(1_000_000, 0).UnixNano()
	for _, key := range []string{"a", "b", "c"} {
		c.markSubGenReleasable(key, t0)
	}

	// Sanity: markers populated, subGen entries still present.
	if len(c.subGenReleaseAt) != 3 {
		t.Fatalf("subGenReleaseAt len = %d, want 3", len(c.subGenReleaseAt))
	}
	if len(c.subGen) != 3 {
		t.Fatalf("subGen len before sweep = %d, want 3", len(c.subGen))
	}

	// A sweep inside the retention window must NOT reclaim: stale resubscribe
	// goroutines may still be parked in resubscribeEvents' 60s ticker.
	// Force the sweep to run by bypassing the throttle with a distant zero
	// lastSweep stamp (already zero); the window check still gates it.
	inWindow := t0 + int64(10*time.Second)
	if n := c.sweepSubGenExpiredLocked(inWindow); n != 0 {
		t.Fatalf("sweep inside retention window reclaimed %d entries, want 0", n)
	}
	if len(c.subGen) != 3 {
		t.Fatalf("subGen len after in-window sweep = %d, want 3 (stale-goroutine contract broken)", len(c.subGen))
	}

	// Advance past retention. Also reset lastSweepNs so the throttle allows
	// the scan — the first sweep above bumped it.
	c.subGenLastSweepNs = 0
	past := t0 + subGenRetentionNanos + int64(time.Second)
	if n := c.sweepSubGenExpiredLocked(past); n != 3 {
		t.Fatalf("sweep past retention reclaimed %d entries, want 3", n)
	}
	if len(c.subGen) != 0 {
		t.Fatalf("subGen after sweep = %d entries, want 0 (reclamation failed)", len(c.subGen))
	}
	if len(c.subGenReleaseAt) != 0 {
		t.Fatalf("subGenReleaseAt after sweep = %d entries, want 0 (marker leak)", len(c.subGenReleaseAt))
	}
}

// TestSubGenReclaim_ActiveSubscriptionPreservesEntry pins the safety net
// inside sweepSubGenExpiredLocked: if a marker somehow outlives a fresh
// subscribe (e.g. a caller forgot to clearSubGenReleasable), the sweep must
// NOT delete the live subGen entry — only drop the stale marker. Losing a
// live subGen[key] would collapse the generation counter and re-expose the
// R163 takeover-detection bug.
func TestSubGenReclaim_ActiveSubscriptionPreservesEntry(t *testing.T) {
	t.Parallel()

	c := &wsClient{
		subscriptions: map[string]func(){
			"active-key": func() {},
		},
		subGen: map[string]uint64{
			"active-key": 5,
		},
	}
	t0 := time.Unix(2_000_000, 0).UnixNano()
	c.markSubGenReleasable("active-key", t0)

	past := t0 + subGenRetentionNanos + int64(time.Second)
	_ = c.sweepSubGenExpiredLocked(past)

	if gen, ok := c.subGen["active-key"]; !ok || gen != 5 {
		t.Fatalf("sweep deleted live subGen entry (got %v, ok=%v) — "+
			"R163 takeover-detection contract broken", gen, ok)
	}
	if _, ok := c.subGenReleaseAt["active-key"]; ok {
		t.Error("stale marker survived sweep for live key — marker-leak regression")
	}
}

// TestSubGenReclaim_ClearReleasableOnFreshSubscribe pins the clear path:
// when a key resubscribes after being marked for release, the marker MUST
// be cleared immediately (completeSubscribe calls clearSubGenReleasable).
// Without this, a sweep triggered mid-life would delete a live subGen
// entry.
func TestSubGenReclaim_ClearReleasableOnFreshSubscribe(t *testing.T) {
	t.Parallel()

	c := &wsClient{
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	t0 := time.Unix(3_000_000, 0).UnixNano()

	// Subscribe, unsubscribe (mark), then resubscribe (clear).
	c.subGen["k"] = 1
	c.markSubGenReleasable("k", t0)
	if _, marked := c.subGenReleaseAt["k"]; !marked {
		t.Fatal("marker was not installed")
	}
	c.clearSubGenReleasable("k")
	if _, marked := c.subGenReleaseAt["k"]; marked {
		t.Error("clearSubGenReleasable did not remove marker")
	}
}

// TestSubGenReclaim_SweepThrottle pins the rate-limit semantics: two sweeps
// within subGenSweepMinIntervalNanos should be a no-op for the second call
// UNLESS the map exceeds the high-water mark.
func TestSubGenReclaim_SweepThrottle(t *testing.T) {
	t.Parallel()

	c := &wsClient{
		subscriptions: make(map[string]func()),
		subGen:        map[string]uint64{"x": 1},
	}
	t0 := time.Unix(4_000_000, 0).UnixNano()
	past := t0 + subGenRetentionNanos + int64(time.Second)

	c.markSubGenReleasable("x", t0)
	if n := c.sweepSubGenExpiredLocked(past); n != 1 {
		t.Fatalf("first sweep reclaimed %d, want 1", n)
	}

	// Second sweep right after should skip because lastSweepNs was bumped.
	// Mark another key but stay within the throttle window.
	c.subGen["y"] = 1
	c.markSubGenReleasable("y", t0)
	soon := past + int64(time.Second) // within 30s throttle
	if n := c.sweepSubGenExpiredLocked(soon); n != 0 {
		t.Errorf("throttled sweep reclaimed %d, want 0 (throttle broken)", n)
	}
	if _, stillThere := c.subGen["y"]; !stillThere {
		t.Error("throttle did not prevent reclamation")
	}
}

// TestSubGenReclaim_HighWaterForcesSweep pins the escape hatch: when the
// marker map grows past subGenHighWaterMark, a sweep MUST run even if the
// throttle would otherwise suppress it. Bounds worst-case memory on
// pathological clients.
func TestSubGenReclaim_HighWaterForcesSweep(t *testing.T) {
	t.Parallel()

	c := &wsClient{
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	t0 := time.Unix(5_000_000, 0).UnixNano()

	// Fill past high-water.
	for i := 0; i < subGenHighWaterMark+10; i++ {
		key := "k" + itoa(i)
		c.subGen[key] = 1
		c.markSubGenReleasable(key, t0)
	}
	// Simulate a recent sweep that would normally suppress.
	c.subGenLastSweepNs = t0 + int64(10*time.Second)

	past := t0 + subGenRetentionNanos + int64(time.Second)
	reclaimed := c.sweepSubGenExpiredLocked(past)
	if reclaimed == 0 {
		t.Fatal("high-water sweep was suppressed — memory bound not enforced")
	}
	if len(c.subGenReleaseAt) != 0 {
		t.Errorf("after high-water sweep, %d markers remain", len(c.subGenReleaseAt))
	}
}

// TestSubGenReclaim_NilMapsSafe ensures the helpers don't panic on a
// freshly-constructed wsClient whose subGenReleaseAt has not yet been
// materialised. mark* lazy-inits; clear* and sweep* tolerate nil.
func TestSubGenReclaim_NilMapsSafe(t *testing.T) {
	t.Parallel()

	c := &wsClient{
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
	}
	// Clear on a nil map must be a no-op, not a panic.
	c.clearSubGenReleasable("nope")
	if n := c.sweepSubGenExpiredLocked(time.Now().UnixNano()); n != 0 {
		t.Errorf("sweep on nil map reclaimed %d, want 0", n)
	}
	// Mark lazily allocates.
	c.markSubGenReleasable("k", time.Now().UnixNano())
	if c.subGenReleaseAt == nil {
		t.Fatal("markSubGenReleasable did not lazy-init subGenReleaseAt")
	}
}

// TestSubGenReclaim_SourceAnchor pins the handleUnsubscribe wiring. A future
// refactor that removes the markSubGenReleasable call would silently
// re-introduce the unbounded accumulation R175-P2 fixed; this source-level
// test trips immediately when the anchor goes missing.
func TestSubGenReclaim_SourceAnchor(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("wshub.go")
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}

	// handleUnsubscribe body must contain markSubGenReleasable + sweep.
	reHandle := regexp.MustCompile(`(?ms)^func \(h \*Hub\) handleUnsubscribe\(.*?\n\}\n`)
	m := reHandle.Find(src)
	if m == nil {
		t.Fatal("could not locate handleUnsubscribe body")
	}
	body := string(m)
	for _, want := range []string{
		"markSubGenReleasable(key",
		"sweepSubGenExpiredLocked",
		"R175-P2",
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(body) {
			t.Errorf("handleUnsubscribe missing anchor %q — R175-P2 reclamation wiring removed?", want)
		}
	}

	// completeSubscribe body must clear a stale marker on the resubscribe
	// path.  Otherwise a sweep mid-life could delete live subGen[key].
	reComplete := regexp.MustCompile(`(?ms)^func \(h \*Hub\) completeSubscribe\(.*?\n\}\n`)
	cm := reComplete.Find(src)
	if cm == nil {
		t.Fatal("could not locate completeSubscribe body")
	}
	if !regexp.MustCompile(`clearSubGenReleasable\(key\)`).MatchString(string(cm)) {
		t.Error("completeSubscribe missing clearSubGenReleasable(key) — " +
			"a sweep mid-life could delete live subGen[key] and re-expose R163.")
	}
}

// itoa is a tiny inline helper to avoid strconv + fmt imports for benchmarks
// and keep the sweep test pure.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
