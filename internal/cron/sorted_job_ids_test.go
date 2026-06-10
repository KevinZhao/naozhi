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
// s.sortedJobIDs. R164029-PERF-9 (#1598).
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

// snapshotSortedIDs reads the maintained slice under the lock so the test
// never races the scheduler's own writers.
func (s *Scheduler) snapshotSortedIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.sortedJobIDs)
}

func (s *Scheduler) sortedMapKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	return keys
}

// TestSortedJobIDs_StayOrderedAcrossMutations pins the #1598 invariant: the
// incrementally-maintained sortedJobIDs slice equals the sorted set of
// s.jobs keys after a sequence of AddJob / DeleteJob mutations, so
// marshalJobsLocked can iterate it without re-sorting in the critical section.
func TestSortedJobIDs_StayOrderedAcrossMutations(t *testing.T) {
	t.Parallel()
	s := newSortedIDsScheduler(t)

	ids := addNJobs(t, s, 8)

	assertMatchesMap := func(stage string) {
		t.Helper()
		got := s.snapshotSortedIDs()
		want := s.sortedMapKeys()
		if !slices.Equal(got, want) {
			t.Fatalf("%s: sortedJobIDs=%v, want sorted map keys=%v", stage, got, want)
		}
		if !slices.IsSorted(got) {
			t.Fatalf("%s: sortedJobIDs not sorted: %v", stage, got)
		}
	}
	assertMatchesMap("after adds")

	// Delete a few jobs by exact ID (prefix match) so the binary-search
	// delete path is exercised against first / middle / last positions.
	for _, id := range []string{ids[0], ids[4], ids[7]} {
		if _, err := s.DeleteJob(id, "", ""); err != nil {
			t.Fatalf("DeleteJob %q: %v", id, err)
		}
	}
	assertMatchesMap("after deletes")

	if got := s.snapshotSortedIDs(); len(got) != 5 {
		t.Fatalf("after 3 deletes from 8: len(sortedJobIDs)=%d, want 5 (%v)", len(got), got)
	}
}

// TestMarshalJobsLocked_UsesSortedHint pins that the marshal output is in
// ascending ID order when the sortedJobIDs hint is in lockstep with s.jobs —
// the production fast path that avoids slices.SortFunc in the s.mu section.
func TestMarshalJobsLocked_UsesSortedHint(t *testing.T) {
	t.Parallel()
	s := newSortedIDsScheduler(t)
	addNJobs(t, s, 5)

	s.mu.RLock()
	got, err := s.marshalJobsLocked()
	s.mu.RUnlock()
	if err != nil {
		t.Fatalf("marshalJobsLocked: %v", err)
	}
	var rt []*Job
	if err := json.Unmarshal(got, &rt); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, got)
	}
	gotOrder := make([]string, len(rt))
	for i, j := range rt {
		gotOrder[i] = j.ID
	}
	if !slices.IsSorted(gotOrder) {
		t.Fatalf("marshal order not sorted by ID: %v", gotOrder)
	}
	if want := s.sortedMapKeys(); !slices.Equal(gotOrder, want) {
		t.Fatalf("marshal order = %v, want all map keys sorted = %v", gotOrder, want)
	}
}

// TestMarshalJobsLocked_DriftFallbackRebuilds pins the correctness guard: if a
// caller pokes s.jobs directly (bypassing the addToChatIndexLocked seam, as a
// handful of in-package test helpers do), the sortedJobIDs hint goes stale.
// marshalJobsLocked MUST detect the drift and rebuild from the map so the
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
	s.mu.Lock()
	s.jobs[directID] = &Job{ID: directID, Schedule: "@every 5m", Prompt: "p"}
	s.mu.Unlock()

	s.mu.RLock()
	got, err := s.marshalJobsLocked()
	s.mu.RUnlock()
	if err != nil {
		t.Fatalf("marshalJobsLocked: %v", err)
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
