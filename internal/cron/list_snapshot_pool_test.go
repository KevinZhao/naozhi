// list_snapshot_pool_test.go: regression coverage for two cron-list snapshot
// invariants that pair across the same code surface (scheduler_jobs.go).
//
//   - R247-PERF-4 (#530) / R250-PERF-15 (#1118): ListAllJobsWithNextRun
//     pools its transient entryID slice + nextByID map. The pool must be
//     reset cleanly between calls so a "second poll" sees no leakage from
//     the first.
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
// unreachable from the recorded entryID slice. This test pins that property as a regression
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

// TestListAllJobsWithNextRun_ValueCopyIsolated guards R250-PERF-15 (#1118).
// The single-copy refactor (Job copied straight into the result slice under
// RLock instead of being copied twice via a pooled scratch) must preserve
// the value-copy isolation: a caller mutating a returned JobWithNextRun.Job
// must NOT corrupt the scheduler's live in-memory *Job. A regression that
// stored the *Job pointer (or aliased the pooled scratch) would surface as
// the live Title changing here.
func TestListAllJobsWithNextRun_ValueCopyIsolated(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   8,
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
		Paused:   true,
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	snap := s.ListAllJobsWithNextRun()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	// Mutate the returned value-copy's Job. With single-copy-into-result
	// semantics this is still a distinct Job value, so the live state must
	// be untouched.
	snap[0].Job.Title = "tampered-by-caller"
	snap[0].Job.RunCounters.Total = 99999

	again := s.ListAllJobsWithNextRun()
	if len(again) != 1 {
		t.Fatalf("second snapshot len = %d, want 1", len(again))
	}
	if again[0].Job.Title != "original" {
		t.Fatalf("live Title = %q, want %q (caller mutation leaked into in-memory state)",
			again[0].Job.Title, "original")
	}
	if again[0].Job.RunCounters.Total != 0 {
		t.Fatalf("live RunCounters.Total = %d, want 0 (caller mutation leaked)",
			again[0].Job.RunCounters.Total)
	}
}

// TestListAllJobsWithNextRun_NextRunByEntryID guards that the index-based
// NextRun patch (R250-PERF-15 #1118) still aligns each result row with the
// correct robfig/cron entry. A non-paused job is registered (so it has a
// live entry with a future Next) and must surface a non-zero NextRun, while
// a paused job (entryID 0, no cron entry) must surface the zero time.
func TestListAllJobsWithNextRun_NextRunByEntryID(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   8,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	active := &Job{
		Schedule: "@every 1h",
		Prompt:   "active",
		Platform: "feishu",
		ChatID:   "chat",
		ChatType: "direct",
		Title:    "active-job",
	}
	if err := s.AddJob(active); err != nil {
		t.Fatalf("AddJob active: %v", err)
	}
	paused := &Job{
		Schedule: "@every 1h",
		Prompt:   "paused",
		Platform: "feishu",
		ChatID:   "chat",
		ChatType: "direct",
		Title:    "paused-job",
		Paused:   true,
	}
	if err := s.AddJob(paused); err != nil {
		t.Fatalf("AddJob paused: %v", err)
	}

	snap := s.ListAllJobsWithNextRun()
	byTitle := map[string]JobWithNextRun{}
	for _, jr := range snap {
		byTitle[jr.Job.Title] = jr
	}
	if got := byTitle["active-job"]; got.NextRun.IsZero() {
		t.Fatalf("active job NextRun is zero, want a scheduled future time")
	}
	if got := byTitle["paused-job"]; !got.NextRun.IsZero() {
		t.Fatalf("paused job NextRun = %v, want zero (no live cron entry)", got.NextRun)
	}
}

// TestNextRun_LockOrderAndResult guards the #1117 lock-order fix: NextRun
// resolves entryID under s.mu.RLock, releases s.mu, then calls
// s.cron.Entry() lock-free. The test asserts the observable result is
// unchanged (registered job → future NextRun; unregistered/zero-entry job
// → zero time) and that concurrent NextRun + CRUD is race-clean under -race
// (a regression that re-took s.mu across cron.Entry would still pass the
// value checks but the -race build pins the lock discipline against a
// future cron.Entry that calls back into scheduler state).
func TestNextRun_LockOrderAndResult(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   8,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	active := &Job{
		Schedule: "@every 1h",
		Prompt:   "p",
		Platform: "feishu",
		ChatID:   "chat",
		ChatType: "direct",
	}
	if err := s.AddJob(active); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if got := s.NextRun(active); got.IsZero() {
		t.Fatalf("NextRun(active) is zero, want a scheduled future time")
	}

	// A Job that never flowed through this scheduler (entryID 0, unknown ID)
	// must return the zero time, not a misleading next run.
	stray := &Job{ID: "deadbeefdeadbeef", Schedule: "@every 1h"}
	if got := s.NextRun(stray); !got.IsZero() {
		t.Fatalf("NextRun(stray) = %v, want zero", got)
	}
	if got := s.NextRun(nil); !got.IsZero() {
		t.Fatalf("NextRun(nil) = %v, want zero", got)
	}

	// Concurrent NextRun while CRUD mutates the scheduler — must not deadlock
	// or race. AddJob/DeleteJob take s.mu.Lock + cron mutations; NextRun now
	// reads cron.Entry outside s.mu, so the two no longer contend in an
	// inverted order.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				_ = s.NextRun(active)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 16; j++ {
			tmp := &Job{
				Schedule: "@every 2h",
				Prompt:   "tmp",
				Platform: "feishu",
				ChatID:   "chat2",
				ChatType: "direct",
			}
			if err := s.AddJob(tmp); err != nil {
				t.Errorf("concurrent AddJob: %v", err)
				return
			}
			if _, err := s.DeleteJobByID(tmp.ID); err != nil {
				t.Errorf("concurrent DeleteJobByID: %v", err)
				return
			}
		}
	}()
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
