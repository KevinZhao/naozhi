package cron

import (
	"errors"
	"path/filepath"
	"testing"
)

// schedulerForJobsR241GO2Test is a minimal Scheduler fixture for
// scheduler_jobs_test.go. It mirrors the simplest NewScheduler bootstrap
// used elsewhere in the package and is local to this file so it does not
// collide with future helpers.
func schedulerForJobsR241GO2Test(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        8,
		AllowNilRouter: true,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

// TestDeleteJobByID_NotFoundReturnsErrJobNotFound_R241_GO_2 is the
// regression test for R241-GO-2 (#488). The original IIFE pattern
// signalled "not found" by returning a nil *Job, which conflicted with
// the latent callsite contract "j is *Job, may be nil for valid Jobs"
// and was correct only by accident. The current implementation routes
// through withJobByIDOpt with an explicit `found bool`, so a missing
// id surfaces as (nil, ErrJobNotFound) — this test pins that contract
// so a future refactor cannot reintroduce the nil-sentinel ambiguity.
func TestDeleteJobByID_NotFoundReturnsErrJobNotFound_R241_GO_2(t *testing.T) {
	t.Parallel()
	s := schedulerForJobsR241GO2Test(t)

	got, err := s.DeleteJobByID("does-not-exist")
	if got != nil {
		t.Fatalf("DeleteJobByID returned non-nil Job %+v on missing id; want nil with ErrJobNotFound", got)
	}
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("DeleteJobByID error = %v; want ErrJobNotFound (no nil-sentinel ambiguity)", err)
	}
}

// TestPauseJobByID_NotFoundReturnsErrJobNotFound_R241_GO_3 is the
// regression test for R241-GO-3 (#488 — same pattern). PauseJobByID
// uses withJobByIDOpt's rollback path so the test also implicitly
// exercises that the rollback shim does not mask not-found as a
// success-with-nil-Job — the explicit found bool dominates.
func TestPauseJobByID_NotFoundReturnsErrJobNotFound_R241_GO_3(t *testing.T) {
	t.Parallel()
	s := schedulerForJobsR241GO2Test(t)

	got, err := s.PauseJobByID("missing")
	if got != nil {
		t.Fatalf("PauseJobByID returned non-nil Job %+v on missing id; want nil with ErrJobNotFound", got)
	}
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("PauseJobByID error = %v; want ErrJobNotFound (no nil-sentinel ambiguity)", err)
	}
}

// TestDeleteJobByID_FoundReturnsSnapshot_R241_GO_2 pins the success
// path: the returned *Job must be a non-nil snapshot of the deleted
// job — proving the explicit found bool routes the success case
// correctly even though `*Job` is the nil-able half of the
// historically-ambiguous contract.
func TestDeleteJobByID_FoundReturnsSnapshot_R241_GO_2(t *testing.T) {
	t.Parallel()
	s := schedulerForJobsR241GO2Test(t)

	j := &Job{
		Schedule: "@every 1h",
		Title:    "delete-snapshot",
		Prompt:   "noop",
		Platform: "feishu",
		ChatID:   "c1",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if j.ID == "" {
		t.Fatal("AddJob did not assign an ID")
	}

	got, err := s.DeleteJobByID(j.ID)
	if err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	if got == nil {
		t.Fatal("DeleteJobByID returned nil Job on success; want snapshot of deleted job")
	}
	if got.ID != j.ID {
		t.Fatalf("snapshot.ID = %q; want %q", got.ID, j.ID)
	}

	// Second call must see ErrJobNotFound — proves the delete actually
	// removed the in-memory entry and the not-found path returns the
	// correct sentinel error rather than a stale nil-Job-no-error pair.
	got2, err2 := s.DeleteJobByID(j.ID)
	if got2 != nil {
		t.Fatalf("second DeleteJobByID returned non-nil Job %+v; want nil", got2)
	}
	if !errors.Is(err2, ErrJobNotFound) {
		t.Fatalf("second DeleteJobByID error = %v; want ErrJobNotFound", err2)
	}
}
