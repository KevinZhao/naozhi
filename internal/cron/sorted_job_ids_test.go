package cron

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// newSortedIDsScheduler spins up a started scheduler with a temp store so the
// AddJob/DeleteJob paths exercise the real persist hot path that maintains
// s.tbl.sortedJobIDs. R164029-PERF-9 (#1598).
func newSortedIDsScheduler(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   50,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

// addNJobs adds n paused jobs via the production AddJob path (which generates
// the real ID) and returns the generated IDs in insertion order.
func addNJobs(t *testing.T, s *Scheduler, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		j := &Job{Schedule: "@every 5m", Prompt: "p", Paused: true}
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob #%d: %v", i, err)
		}
		ids = append(ids, j.ID)
	}
	return ids
}

// TestMarshalJobsLocked_DriftFallbackRebuilds pins the correctness guard: if a
// caller pokes s.tbl.jobs directly (bypassing the indexLocked seam, as a
// handful of in-package test helpers do), the sortedJobIDs hint goes stale.
// marshalLocked MUST detect the drift and rebuild from the map so the
// direct-inserted job is never silently dropped from the on-disk snapshot.
func TestMarshalJobsLocked_DriftFallbackRebuilds(t *testing.T) {
	t.Parallel()
	s := newSortedIDsScheduler(t)

	// One job via the normal seam (lands in sortedJobIDs)...
	seamIDs := addNJobs(t, s, 1)
	// ...and one poked directly into the map (NOT in sortedJobIDs), forcing a
	// length mismatch so the hint path is rejected. "a-direct" sorts before
	// any hex8 ID, so a correct rebuild puts it first.
	const directID = "a-direct-id-000"
	s.putUnindexedJobForTest(&Job{ID: directID, Schedule: "@every 5m", Prompt: "p"})
	if slices.Contains(s.tbl.ids(), directID) || !slices.Contains(s.mapKeysForTest(), directID) {
		t.Fatal("precondition: the direct-poked job must be in the map but not the sorted-ID index")
	}

	got, err := s.marshalForTest()
	if err != nil {
		t.Fatalf("marshalLocked: %v", err)
	}
	var rt []*Job
	if err := json.Unmarshal(got, &rt); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, got)
	}
	gotIDs := make([]string, len(rt))
	for i, j := range rt {
		gotIDs[i] = j.ID
	}
	// Both jobs must appear (drift must NOT drop the direct-poked one), and
	// the fallback re-sorts so output stays deterministic by ID.
	want := []string{directID, seamIDs[0]}
	sort.Strings(want)
	if !slices.IsSorted(gotIDs) {
		t.Fatalf("drift fallback output not sorted by ID: %v", gotIDs)
	}
	if !slices.Equal(gotIDs, want) {
		t.Fatalf("drift fallback dropped/misordered jobs: got %v, want %v", gotIDs, want)
	}
}
