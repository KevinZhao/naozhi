package session

import "testing"

// TestListSessionsWithVersion_PoolRetainsGrownSlice is the regression guard for
// #2309: when the session count exceeds the pooled slice's capacity,
// ListSessionsWithVersion allocates a larger backing array. The grown slice
// must be written back into the pooled *[]*ManagedSession (via the
// `*refsPtr = refs[:0]` store before Put) so the pool keeps recycling the
// larger buffer instead of degrading to "allocate once, never reuse" after a
// deployment-scale bump.
// The assertion measures steady-state ALLOCATIONS rather than inspecting the
// pool's contents.
//
// The original version asserted via a second listRefsPool.Get() and was flaky
// (it failed on the macOS CI runner with "cap = 64 after grow"): sync.Pool makes
// no promise about WHICH object Get returns — it is per-P, steals across Ps, and
// GC can drop pooled items — so Get can legitimately hand back a fresh New()
// cap-64 slice even when the write-back worked. Seeding a pointer and watching
// it fails for the mirror reason: the pool may hand the call some OTHER pooled
// slice, leaving the seed untouched, which is indistinguishable from a genuine
// regression.
//
// What #2309 is really about is cost. With the write-back a warmed pool recycles
// the grown buffer and each poll allocates only the snapshots slice; without it
// every poll re-enters the grow branch and pays an extra
// make([]*ManagedSession, 0, n). Measured on this fixture: 1.0 vs 2.0, both with
// and without -race. Comparing against a same-process baseline (a session set
// that fits the default pool capacity, so it never grows) rather than a
// hard-coded 1.0 keeps that valid under -race and under future changes to what
// else the call allocates.
func TestListSessionsWithVersion_PoolRetainsGrownSlice(t *testing.T) {
	// Skipped under the race detector for the same reason as
	// TestMarshalStoreEntries_AllocsDoNotScaleWithN (store_marshal_pool_test.go):
	// -race rewrites allocation paths and the shared sync.Pool is drained and
	// refilled by concurrently-running tests, so AllocsPerRun is not
	// deterministic. Measured 7/30 spurious failures under `-count=30 -race`
	// before this guard; the non-race run still covers the invariant on every CI
	// job (`test` and `test-macos` both run without -race).
	if raceEnabled {
		t.Skip("AllocsPerRun is unreliable under -race + shared sync.Pool")
	}
	// Not parallel: it drains the package-global listRefsPool, which a
	// concurrent test using ListSessions* would otherwise refill mid-measurement.
	const fits = 10 // < default pool cap of 64, never enters the grow branch
	baseline := steadyStateAllocs(t, fits)

	const n = 200 // > default pool cap of 64, forces the grow branch
	grown := steadyStateAllocs(t, n)

	// With the write-back both are equal (the grown buffer is recycled). Without
	// it the grown case pays one extra make([]*ManagedSession, 0, n) per poll.
	if grown > baseline+0.5 {
		t.Errorf("steady-state allocs/call = %.1f for %d sessions vs %.1f baseline (%d sessions) — "+
			"the grown refs slice is not being written back through refsPtr, so every poll re-grows it",
			grown, n, baseline, fits)
	}
}

// steadyStateAllocs builds a router holding n sessions, warms the pool so the
// unavoidable first grow is not counted, and returns allocations per
// ListSessionsWithVersion call.
//
// It drains listRefsPool first. Without that, a grown buffer left over from an
// earlier measurement (or an earlier run under `-count=N`) gets recycled by the
// small-session router too, lifting the baseline to match the grown case and
// making the comparison below pass or fail at random — observed as 4/20 failures
// under `-count=20 -race`.
func steadyStateAllocs(t *testing.T, n int) float64 {
	t.Helper()
	drainListRefsPool()

	r := NewRouter(RouterConfig{MaxProcs: 0, TTL: 0})
	t.Cleanup(func() { r.Shutdown() })

	r.mu.Lock()
	r.ss.sessions = make(map[string]*ManagedSession, n)
	for i := 0; i < n; i++ {
		key := keyOf(i)
		r.ss.sessions[key] = newSessionWithID(key, "id")
	}
	r.mu.Unlock()

	for i := 0; i < 20; i++ {
		snaps, _ := r.ListSessionsWithVersion()
		if len(snaps) != n {
			t.Fatalf("snapshot count = %d, want %d", len(snaps), n)
		}
	}
	return testing.AllocsPerRun(50, func() { r.ListSessionsWithVersion() })
}

// drainListRefsPool discards pooled slices until Get consistently yields a
// fresh New() slice (cap 64), i.e. the pool is empty. Bounded so a pool being
// refilled concurrently cannot spin forever; sync.Pool is per-P and lossy, so
// "empty" is best-effort by construction — 20 consecutive default-cap slices is
// a strong enough signal, and the assertion only needs the two measurements to
// start from the same state.
func drainListRefsPool() {
	const (
		maxGets     = 1000
		defaultRuns = 20
	)
	fresh := 0
	for i := 0; i < maxGets && fresh < defaultRuns; i++ {
		p := listRefsPool.Get().(*[]*ManagedSession)
		if cap(*p) == 64 {
			fresh++
		} else {
			fresh = 0
		}
	}
}

func keyOf(i int) string {
	return "dashboard:direct:proj:agent-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
