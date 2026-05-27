package cron

// R20260527-SEC-4 (#1273) regression tests.
//
// workDirResolveCache must:
//   - bound entry count via workDirResolveCacheMaxEntries;
//   - sweep expired entries when at-or-above the cap;
//   - drop the new insert when the cap is still exceeded after sweep
//     (correctness-safe: next call simply re-runs EvalSymlinks);
//   - decrement the size counter on lookup-driven expiry so the cap
//     doesn't drift upward across many partial-overlap insert/expire
//     cycles.

import (
	"strconv"
	"testing"
	"time"
)

// TestWorkDirResolveCache_CapBoundsEntryCount proves a misbehaving caller
// that varies WorkDir per call cannot grow the map without bound. We
// drive size to the cap with fresh keys, then assert that the next
// fresh-key Store does NOT add a (cap+1)'th entry. The pre-fix shape
// would happily Store unbounded entries via sync.Map.Store.
func TestWorkDirResolveCache_CapBoundsEntryCount(t *testing.T) {
	t.Parallel()
	c := &workDirResolveCache{}
	now := time.Now()

	// Fill to the cap with non-expired entries.
	for i := 0; i < workDirResolveCacheMaxEntries; i++ {
		c.store("k-"+strconv.Itoa(i), "/r-"+strconv.Itoa(i), now)
	}
	if got := c.size.Load(); got != workDirResolveCacheMaxEntries {
		t.Fatalf("after fill: size=%d want=%d", got, workDirResolveCacheMaxEntries)
	}

	// Try to insert one more fresh key with the same now — sweep finds
	// nothing expired, cap holds, insert is dropped.
	c.store("overflow-key", "/r-overflow", now)
	if got := c.size.Load(); got > workDirResolveCacheMaxEntries {
		t.Fatalf("after overflow Store: size=%d > cap=%d (cap not enforced)",
			got, workDirResolveCacheMaxEntries)
	}
	if _, ok := c.lookup("overflow-key", now); ok {
		t.Fatalf("overflow-key should NOT be cached when cap is full and no entries are expired")
	}
}

// TestWorkDirResolveCache_SweepDropsExpired confirms that on cap-overflow
// the sweep prunes expired entries and the new insert succeeds when
// headroom opens up.
func TestWorkDirResolveCache_SweepDropsExpired(t *testing.T) {
	t.Parallel()
	c := &workDirResolveCache{}
	old := time.Now()

	// Half the cap with stale entries (will expire well before "now").
	half := workDirResolveCacheMaxEntries / 2
	for i := 0; i < half; i++ {
		c.store("old-"+strconv.Itoa(i), "/r-"+strconv.Itoa(i), old)
	}
	// Other half with fresh entries at a much later "now".
	now := old.Add(2 * workDirResolveCacheTTL)
	for i := 0; i < half; i++ {
		c.store("fresh-"+strconv.Itoa(i), "/r-"+strconv.Itoa(i), now)
	}
	// Top up to the cap with one more fresh entry so we are exactly at cap.
	for i := half; i < workDirResolveCacheMaxEntries-half; i++ {
		c.store("topup-"+strconv.Itoa(i), "/r-"+strconv.Itoa(i), now)
	}
	if got := c.size.Load(); got != workDirResolveCacheMaxEntries {
		t.Fatalf("pre-overflow: size=%d want=%d", got, workDirResolveCacheMaxEntries)
	}

	// New fresh insert should trigger sweep, which drops the `old-*`
	// entries (expired at `now` because `now > old + TTL`), leaving
	// headroom for the new insert.
	c.store("newcomer", "/r-new", now)

	if got := c.size.Load(); got > workDirResolveCacheMaxEntries {
		t.Fatalf("post-sweep size=%d > cap=%d", got, workDirResolveCacheMaxEntries)
	}
	if v, ok := c.lookup("newcomer", now); !ok || v != "/r-new" {
		t.Fatalf("newcomer should be cached after sweep; got v=%q ok=%v", v, ok)
	}
	// At least one of the old- entries must be gone.
	stillThere := 0
	for i := 0; i < half; i++ {
		if _, ok := c.lookup("old-"+strconv.Itoa(i), now); ok {
			stillThere++
		}
	}
	if stillThere == half {
		t.Fatalf("sweep did not drop any expired entries; stillThere=%d half=%d", stillThere, half)
	}
}

// TestWorkDirResolveCache_LookupDecrementsSize ensures the lookup-driven
// lazy expiry path keeps `size` in sync. Without the decrement, a long
// run of "store at t0 → lookup at t0+TTL" would leave size pinned at the
// fill level even though Delete fired, eventually wedging the cap path.
func TestWorkDirResolveCache_LookupDecrementsSize(t *testing.T) {
	t.Parallel()
	c := &workDirResolveCache{}
	t0 := time.Now()
	for i := 0; i < 10; i++ {
		c.store("k-"+strconv.Itoa(i), "/r-"+strconv.Itoa(i), t0)
	}
	if got := c.size.Load(); got != 10 {
		t.Fatalf("after fill: size=%d want=10", got)
	}
	// Probe each at TTL boundary — drives lazy Delete + size decrement.
	expired := t0.Add(workDirResolveCacheTTL)
	for i := 0; i < 10; i++ {
		if _, ok := c.lookup("k-"+strconv.Itoa(i), expired); ok {
			t.Fatalf("k-%d should be expired at TTL boundary", i)
		}
	}
	if got := c.size.Load(); got != 0 {
		t.Fatalf("after lazy-expire walk: size=%d want=0 — decrement leaked", got)
	}
}

// TestWorkDirResolveCache_OverwriteDoesNotGrowSize pins that re-storing
// a key already in the cache does NOT inflate the size counter. The
// implementation uses sync.Map.Swap and only Adds(1) when the key was
// not already present.
func TestWorkDirResolveCache_OverwriteDoesNotGrowSize(t *testing.T) {
	t.Parallel()
	c := &workDirResolveCache{}
	now := time.Now()
	c.store("k", "/r1", now)
	if got := c.size.Load(); got != 1 {
		t.Fatalf("initial: size=%d want=1", got)
	}
	// Overwrite with a different value — size must stay 1.
	c.store("k", "/r2", now.Add(time.Second))
	if got := c.size.Load(); got != 1 {
		t.Fatalf("after overwrite: size=%d want=1 (Swap is being treated as fresh insert)", got)
	}
	if v, ok := c.lookup("k", now.Add(time.Second)); !ok || v != "/r2" {
		t.Fatalf("after overwrite: got v=%q ok=%v want=/r2 ok=true", v, ok)
	}
}
