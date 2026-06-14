package cron

import (
	"path/filepath"
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

// TestRunStore_RecentSessionIDs_DedupWarmCache pins the godoc "distinct"
// contract on the cache-warm fast path: a job whose runs reuse the same
// persistent SessionID across multiple runs must return that ID once, not
// once per run (R20260614-LOGIC-3).
func TestRunStore_RecentSessionIDs_DedupWarmCache(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	// Three runs reuse one session, a fourth uses a second session.
	for i, sid := range []string{"sess-A", "sess-A", "sess-A", "sess-B"} {
		r := makeRun(jobID, now.Add(time.Duration(i)*time.Second))
		r.SessionID = sid
		s.Append(r)
	}
	_ = s.List(jobID, 10, time.Time{}) // warm the cache

	got := s.RecentSessionIDs(jobID, 10)
	// Core contract: exactly the two distinct IDs, each once (no per-run
	// duplicate). Ordering of the warm ring after a List-driven warmCache mixes
	// append-prepend and disk-sort order, so assert the deduped SET, not a
	// brittle index order (the cold-path test pins deterministic order).
	if len(got) != 2 {
		t.Fatalf("RecentSessionIDs len=%d want 2 distinct: %v", len(got), got)
	}
	seen := map[string]int{}
	for _, sid := range got {
		seen[sid]++
	}
	if seen["sess-A"] != 1 || seen["sess-B"] != 1 {
		t.Fatalf("RecentSessionIDs = %v want each of sess-A, sess-B exactly once", got)
	}
}

// TestRunStore_RecentSessionIDs_DedupColdPath pins the same "distinct"
// contract on the cold (disk) path. A second runStore opened over the same
// store dir starts cache-empty, so RecentSessionIDs falls back to the
// List+filter branch — which must also dedup (R20260614-LOGIC-3).
func TestRunStore_RecentSessionIDs_DedupColdPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	writer := newRunStore(storePath, 200, 30*24*time.Hour)
	if writer == nil || writer.disabled {
		t.Fatalf("newRunStore writer disabled/nil")
	}
	jobID := mustGenerateID()
	now := time.Now()
	for i, sid := range []string{"dup", "dup", "other"} {
		r := makeRun(jobID, now.Add(time.Duration(i)*time.Second))
		r.SessionID = sid
		writer.Append(r)
	}

	// Fresh store over the same dir: empty recentCache forces the cold path.
	reader := newRunStore(storePath, 200, 30*24*time.Hour)
	if reader == nil || reader.disabled {
		t.Fatalf("newRunStore reader disabled/nil")
	}
	if _, ok := reader.recentCache.Load(jobID); ok {
		t.Fatalf("reader cache unexpectedly warm; cold path not exercised")
	}
	got := reader.RecentSessionIDs(jobID, 10)
	// Core contract: the duplicated "dup" collapses to one entry; "other"
	// appears once. Assert the deduped SET (disk-sort order between same-
	// second runs is not the property under test here).
	if len(got) != 2 {
		t.Fatalf("cold-path RecentSessionIDs len=%d want 2 distinct: %v", len(got), got)
	}
	seen := map[string]int{}
	for _, sid := range got {
		seen[sid]++
	}
	if seen["dup"] != 1 || seen["other"] != 1 {
		t.Fatalf("cold-path RecentSessionIDs = %v want each of dup, other exactly once", got)
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
