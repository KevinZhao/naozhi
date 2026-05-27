// list_snapshot_pool_test.go: regression coverage for two cron-list snapshot
// invariants that pair across the same code surface (scheduler_jobs.go).
//
//   - R247-PERF-4 (#530): ListAllJobsWithNextRun pools its transient pairs
//     slice + nextByID map. The pool must be reset cleanly between calls so
//     a "second poll" sees no leakage from the first.
//   - R242-GO-3 (#548): withJobByID returns a value-copy *Job. A caller
//     that mutates the returned pointer's fields must NOT influence the
//     scheduler's in-memory state (the live *Job in s.jobs).
//
// Both checks are race-free under -race because they exercise the same
// public surface the dashboard does: NewScheduler → AddJob → public read
// API → tear-down via Stop().

package cron

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestListAllJobsWithNextRun_PoolReusable poll-loops the dashboard list
// API and asserts each call returns a self-consistent snapshot regardless
// of how many prior calls have warmed the sync.Pool path. Without
// `clear(nextByID)` before Put, a smaller second snapshot could observe
// stale EntryID keys from a larger first snapshot — but that map is
// keyed by entryID returned from `s.cron.Entries()` so a stale key is
// unreachable from `pairs`. This test pins that property as a regression
// guard so future refactors don't reintroduce a leak via a different key
// shape (e.g. switching to job ID).
func TestListAllJobsWithNextRun_PoolReusable(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   16,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	// Seed three jobs (paused, so no real cron entries get scheduled — we
	// only care about snapshot fidelity, not run-time dispatch).
	for i := 0; i < 3; i++ {
		j := &Job{
			Schedule: "@every 1h",
			Prompt:   "p",
			Platform: "feishu",
			ChatID:   "chat",
			ChatType: "direct",
			Paused:   true,
		}
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob[%d]: %v", i, err)
		}
	}

	// Two sequential polls: cap of pooled pairs grows on the first call;
	// the second call must still produce a slice of length 3.
	got1 := s.ListAllJobsWithNextRun()
	got2 := s.ListAllJobsWithNextRun()
	if len(got1) != 3 || len(got2) != 3 {
		t.Fatalf("snapshot len: got1=%d got2=%d, want 3 / 3", len(got1), len(got2))
	}

	// Concurrent pollers must each see a valid snapshot — pool Get/Put is
	// not racy under sync.Pool's own contract, but our defer-Put dance
	// must remain race-free.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				snap := s.ListAllJobsWithNextRun()
				if len(snap) != 3 {
					t.Errorf("concurrent snapshot len = %d, want 3", len(snap))
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestWithJobByID_ReturnsValueCopy guards R242-GO-3 (#548). The pointer
// returned from DeleteJobByID/PauseJobByID/ResumeJobByID must NOT share
// memory with the live *Job in s.jobs — caller mutations on the returned
// pointer must not surface inside the scheduler's locked state.
//
// Pause/Resume keep the entry in s.jobs after the call, so we exercise
// PauseJobByID and verify a caller-side write to the returned pointer's
// Title doesn't propagate to a subsequent ListJobs read.
func TestWithJobByID_ReturnsValueCopy(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   4,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	j := &Job{
		Schedule: "@every 1h",
		Prompt:   "p",
		Platform: "feishu",
		ChatID:   "chat",
		ChatType: "direct",
		Title:    "original",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	got, err := s.PauseJobByID(j.ID)
	if err != nil {
		t.Fatalf("PauseJobByID: %v", err)
	}
	// Mutate the returned pointer — value-copy semantics mean the live
	// *Job in s.jobs must still report the original Title.
	got.Title = "tampered-by-caller"

	live := s.ListJobs("feishu", "chat")
	if len(live) != 1 {
		t.Fatalf("ListJobs len = %d, want 1", len(live))
	}
	if live[0].Title != "original" {
		t.Fatalf("live Title = %q, want %q (caller-side mutation leaked into in-memory state)",
			live[0].Title, "original")
	}
}
