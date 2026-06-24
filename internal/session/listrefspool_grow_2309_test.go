package session

import "testing"

// TestListSessionsWithVersion_PoolRetainsGrownSlice is the regression guard for
// #2309: when the session count exceeds the pooled slice's capacity,
// ListSessionsWithVersion allocates a larger backing array. The grown slice
// must be written back into the pooled *[]*ManagedSession (via the
// `*refsPtr = refs[:0]` store before Put) so the pool keeps recycling the
// larger buffer instead of degrading to "allocate once, never reuse" after a
// deployment-scale bump.
func TestListSessionsWithVersion_PoolRetainsGrownSlice(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 0, TTL: 0})
	t.Cleanup(func() { r.Shutdown() })

	// Drain the pool so the first call below pulls the default New() slice
	// (cap 64) and is forced through the grow branch by the large session set.
	listRefsPool.Put(func() *[]*ManagedSession {
		s := make([]*ManagedSession, 0, 64)
		return &s
	}())
	got := listRefsPool.Get().(*[]*ManagedSession)
	startCap := cap(*got)
	listRefsPool.Put(got)

	const n = 200 // > default pool cap of 64, forces the grow branch
	r.mu.Lock()
	r.ss.sessions = make(map[string]*ManagedSession, n)
	for i := 0; i < n; i++ {
		key := keyOf(i)
		r.ss.sessions[key] = newSessionWithID(key, "id")
	}
	r.mu.Unlock()

	snaps, _ := r.ListSessionsWithVersion()
	if len(snaps) != n {
		t.Fatalf("snapshot count = %d, want %d", len(snaps), n)
	}

	// After the call returns, the pooled slice must have been grown to at least
	// n. If the grown slice were dropped (the bug), the pool would still hold
	// the original startCap-sized buffer.
	pooled := listRefsPool.Get().(*[]*ManagedSession)
	defer listRefsPool.Put(pooled)
	if cap(*pooled) < n {
		t.Fatalf("pooled slice cap = %d after grow, want >= %d (grown slice not returned to pool; started at %d)",
			cap(*pooled), n, startCap)
	}
	if len(*pooled) != 0 {
		t.Fatalf("pooled slice len = %d, want 0 (must be reset before Put)", len(*pooled))
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
