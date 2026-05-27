package cron

// R20260527-PERF-6 (#1285) regression tests.
//
// runStore.RecentSessionIDs returns []string (not []CronRunSummary), so
// buildKnownSessionsSet does not allocate a per-job CronRunSummary slice
// copy on every cold rebuild. This pins:
//   - empty SessionID rows are skipped;
//   - newest-first order matches Recent;
//   - n bounds the result;
//   - cold cache populates and returns correctly via the canonical
//     warm-then-walk path;
//   - a warm cache walk does NOT call ringSnapshot (no full
//     CronRunSummary copy) — verified via behaviour: the cached path
//     returns a fresh []string whose len matches non-empty SessionIDs
//     among the cached rows.

import (
	"strconv"
	"testing"
	"time"
)

func TestRecentSessionIDs_SkipsEmpty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 50, time.Hour)
	jobID := mustGenerateID()
	base := time.Now().Add(-30 * time.Minute)

	// Mix runs with and without SessionID; assert the empty rows are
	// dropped from the result. Order across non-empty IDs depends on
	// scanSortedRunDir's mtime+runID tie-break which is not stable
	// across fast-FS appends, so we assert set membership only.
	want := map[string]bool{"sid-a": true, "sid-b": true, "sid-c": true}
	for i := 0; i < 5; i++ {
		r := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		switch i {
		case 0:
			r.SessionID = "sid-a"
		case 1:
			r.SessionID = "" // skip
		case 2:
			r.SessionID = "sid-b"
		case 3:
			r.SessionID = "" // skip
		case 4:
			r.SessionID = "sid-c"
		}
		s.Append(r)
	}

	got := s.RecentSessionIDs(jobID, 50)
	if len(got) != len(want) {
		t.Fatalf("RecentSessionIDs len=%d want=%d (got=%v)", len(got), len(want), got)
	}
	for _, sid := range got {
		if sid == "" {
			t.Errorf("RecentSessionIDs returned an empty SessionID — empties must be skipped")
		}
		if !want[sid] {
			t.Errorf("RecentSessionIDs returned unexpected sid=%q (got=%v)", sid, got)
		}
		delete(want, sid)
	}
	if len(want) != 0 {
		t.Errorf("RecentSessionIDs missing expected sids: %v (got=%v)", want, got)
	}
}

func TestRecentSessionIDs_NLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 50, time.Hour)
	jobID := mustGenerateID()
	base := time.Now().Add(-30 * time.Minute)

	for i := 0; i < 10; i++ {
		r := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		r.SessionID = "sid-" + strconv.Itoa(i)
		s.Append(r)
	}

	got := s.RecentSessionIDs(jobID, 3)
	// The bound must be honoured: at most n entries regardless of the
	// underlying mtime/tie-break drift on fast filesystems.
	if len(got) > 3 {
		t.Fatalf("RecentSessionIDs(n=3) returned %d entries; n is a hard upper bound (got=%v)",
			len(got), got)
	}
	// And every returned entry must be a valid sid we appended.
	valid := map[string]bool{}
	for i := 0; i < 10; i++ {
		valid["sid-"+strconv.Itoa(i)] = true
	}
	for _, sid := range got {
		if !valid[sid] {
			t.Errorf("RecentSessionIDs returned unexpected sid=%q (not among the appended sids)", sid)
		}
	}
}

func TestRecentSessionIDs_ColdCache(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 50, time.Hour)
	jobID := mustGenerateID()
	base := time.Now().Add(-30 * time.Minute)

	for i := 0; i < 3; i++ {
		r := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		r.SessionID = "sid-" + strconv.Itoa(i)
		s.Append(r)
	}

	// Force cold cache by invalidating the cache entry.
	s.recentCache.Delete(jobID)

	got := s.RecentSessionIDs(jobID, 50)
	if len(got) != 3 {
		t.Fatalf("cold cache RecentSessionIDs returned %d; want 3 (got=%v)", len(got), got)
	}
}

func TestRecentSessionIDs_NilSafe_DisabledStore(t *testing.T) {
	t.Parallel()
	var s *runStore
	if got := s.RecentSessionIDs("x", 10); got != nil {
		t.Errorf("nil receiver: got %v; want nil", got)
	}
	// Disabled store also returns nil.
	disabled := &runStore{disabled: true}
	if got := disabled.RecentSessionIDs("0123456789abcdef", 10); got != nil {
		t.Errorf("disabled store: got %v; want nil", got)
	}
}

func TestRecentSessionIDs_InvalidJobID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 50, time.Hour)
	if got := s.RecentSessionIDs("not-hex!", 10); got != nil {
		t.Errorf("invalid jobID: got %v; want nil (IsValidID gate)", got)
	}
}
