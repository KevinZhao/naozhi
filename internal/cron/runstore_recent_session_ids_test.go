package cron

import (
	"testing"
	"time"
)

// TestRunStore_RecentSessionIDs_WarmCache covers the cache-warm fast path
// added by R20260527-PERF-6 (#1285). After Append populates the head ring
// every SessionID seeded into a CronRun must surface in the returned slice
// in newest-first order, and rows with empty SessionID must be omitted.
func TestRunStore_RecentSessionIDs_WarmCache(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	r1 := makeRun(jobID, now.Add(-3*time.Second))
	r1.SessionID = "sess-oldest"
	s.Append(r1)
	r2 := makeRun(jobID, now.Add(-2*time.Second))
	r2.SessionID = "" // intentionally empty — must be filtered
	s.Append(r2)
	r3 := makeRun(jobID, now.Add(-1*time.Second))
	r3.SessionID = "sess-newest"
	s.Append(r3)

	// Prime the cache via List so the entry is warm before RecentSessionIDs
	// hits the fast path.
	_ = s.List(jobID, 10, time.Time{})

	got := s.RecentSessionIDs(jobID, 10)
	if len(got) != 2 {
		t.Fatalf("RecentSessionIDs len=%d want 2: %v", len(got), got)
	}
	// Newest-first ordering: cacheHeadPush prepends so r3 is at index 0.
	if got[0] != "sess-newest" || got[1] != "sess-oldest" {
		t.Fatalf("RecentSessionIDs order = %v want [sess-newest sess-oldest]", got)
	}
}

// TestRunStore_RecentSessionIDs_LimitClamp verifies n is clamped against
// the in-cache count (no out-of-bounds index when n exceeds the ring).
func TestRunStore_RecentSessionIDs_LimitClamp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	r := makeRun(jobID, time.Now())
	r.SessionID = "only"
	s.Append(r)
	_ = s.List(jobID, 10, time.Time{})

	got := s.RecentSessionIDs(jobID, 100)
	if len(got) != 1 || got[0] != "only" {
		t.Fatalf("RecentSessionIDs = %v want [only]", got)
	}
}

// TestRunStore_RecentSessionIDs_NilSafe / disabled / invalid jobID early
// returns mirror Recent's contract and prevent the new method from being
// a panic surface for nil-receiver callers (parity with Recent).
func TestRunStore_RecentSessionIDs_NilSafe(t *testing.T) {
	t.Parallel()
	var s *runStore
	if got := s.RecentSessionIDs("abcd", 10); got != nil {
		t.Fatalf("nil receiver returned non-nil: %v", got)
	}
	live := newTestStore(t, 200, time.Hour)
	if got := live.RecentSessionIDs("", 10); got != nil {
		t.Fatalf("empty jobID returned non-nil: %v", got)
	}
	if got := live.RecentSessionIDs("NOT-HEX", 10); got != nil {
		t.Fatalf("invalid jobID returned non-nil: %v", got)
	}
}
