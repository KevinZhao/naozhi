package cron

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestMarshalJobsLocked_SkipSortEmpty pins the #482 fast-path: an empty
// jobs map marshals to `null` (the json.Marshal contract for a nil
// slice argument) without invoking SortFunc. We can't observe "did the
// sort run" directly, but we can pin the on-disk shape so a future
// refactor that re-introduces an unconditional sort still has to
// preserve the byte-output contract. cron_jobs.json is regenerated at
// startup from this exact shape, so the empty-scheduler payload is a
// load-bearing fixture (NewScheduler → first AddJob path).
func TestMarshalJobsLocked_SkipSortEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	s.mu.RLock()
	defer s.mu.RUnlock()
	got, err := s.marshalJobsLocked()
	if err != nil {
		t.Fatalf("marshalJobsLocked: %v", err)
	}
	// json.Marshal of a nil/empty []*Job emits "null" (not "[]") because
	// json.NewEncoder + nil slice → null. Pin both shapes since either
	// is acceptable for an empty cron — the key invariant is that the
	// fast path produces the same output unconditional sort would.
	str := string(got)
	if str != "null" && str != "[]" {
		t.Fatalf("empty marshal = %q; want \"null\" or \"[]\"", str)
	}
	// Ensure the bytes are valid JSON (defensive — catches a future
	// regression that returns garbage on the empty fast path).
	var probe any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatalf("unmarshal %q: %v", str, err)
	}
}

// TestMarshalJobsLocked_SkipSortSingle covers the len==1 fast-path
// branch (#482): a single job marshals deterministically without
// going through slices.SortFunc. Output must round-trip and contain
// the job's ID.
func TestMarshalJobsLocked_SkipSortSingle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	jobID := mustGenerateID()
	s.mu.Lock()
	s.jobs[jobID] = &Job{ID: jobID, Schedule: "@every 1m", Prompt: "p"}
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
	if len(rt) != 1 || rt[0].ID != jobID {
		t.Fatalf("round trip mismatch: %+v", rt)
	}
}
