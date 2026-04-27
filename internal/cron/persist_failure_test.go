package cron

// R51-QUAL-001 regression tests. persistJobsLocked used to return a silent
// no-op func on marshal failure; every mutation API then reported success
// while nothing reached disk. A process restart replayed stale state —
// "deleted" jobs came back, "paused" jobs started firing, etc.
//
// These tests swap the package-level marshalJobs serializer for one that
// returns an error and confirm each mutation API surfaces ErrPersistFailed
// instead of the previous silent success.

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// withFailingMarshal swaps marshalJobs to a stub that always errors, then
// restores the original on test cleanup. Centralised so each mutation case
// stays focused on its assertion and new callers inherit the same cleanup.
func withFailingMarshal(t *testing.T) {
	t.Helper()
	orig := marshalJobs
	marshalJobs = func(any) ([]byte, error) {
		return nil, fmt.Errorf("injected marshal failure")
	}
	t.Cleanup(func() { marshalJobs = orig })
}

// newTestSchedulerForPersist sets up a Scheduler + one pre-registered job
// (through the real AddJob path BEFORE the marshal stub is installed) so the
// persist-failure assertions fire against existing in-memory state. Caller
// is responsible for installing the failing marshaler after getting the
// job ID and before invoking the mutation under test.
func newTestSchedulerForPersist(t *testing.T) (*Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	j := &Job{
		Schedule: "@every 1h",
		Prompt:   "test",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true, // avoid registering a live cron entry for speed
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob seed: %v", err)
	}
	return s, j.ID
}

func TestPersistFailure_AddJob(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	withFailingMarshal(t)

	err := s.AddJob(&Job{
		Schedule: "@every 1h",
		Prompt:   "test",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true,
	})
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("AddJob err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_DeleteJobByID(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	withFailingMarshal(t)

	_, err := s.DeleteJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("DeleteJobByID err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_DeleteJobByPrefix(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	withFailingMarshal(t)

	_, err := s.DeleteJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("DeleteJob err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_PauseJobByID(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seeded job is Paused=true; resume it first (real path) so the pause
	// call under test actually changes state. Cleanup of withFailingMarshal
	// happens at test end, so we install the stub after Resume.
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID seed: %v", err)
	}

	withFailingMarshal(t)

	_, err := s.PauseJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJobByID err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_ResumeJobByID(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed starts Paused=true, so Resume is the natural mutation.

	withFailingMarshal(t)

	_, err := s.ResumeJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJobByID err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_PauseJobByPrefix(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID seed: %v", err)
	}

	withFailingMarshal(t)

	_, err := s.PauseJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJob err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_ResumeJobByPrefix(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	withFailingMarshal(t)

	_, err := s.ResumeJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJob err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_UpdateJob(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	withFailingMarshal(t)

	newPrompt := "updated"
	_, err := s.UpdateJob(id, JobUpdate{Prompt: &newPrompt})
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("UpdateJob err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_SetJobPrompt(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// SetJobPrompt is only meaningful on a job with empty prompt + paused=true
	// (the dashboard-created placeholder). AddJob rejects empty prompts up
	// front, so we inject the job directly into s.jobs mirroring the flow
	// used by the dashboard placeholder path.
	j := &Job{
		ID:       "abcd1234",
		Schedule: "@every 1h",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true,
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	withFailingMarshal(t)

	err := s.SetJobPrompt(j.ID, "filled in")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("SetJobPrompt err = %v, want ErrPersistFailed", err)
	}
}

// TestPersistFailure_RecordResultSwallowed documents the intentional asymmetry
// at the execute/recordResult path: recordResult has no error return (it runs
// from the internal cron goroutine), so marshal failure there can only be
// logged. The test exercises the path through persistJobsLocked directly to
// lock in the (nil, err) return shape so a future refactor that changes
// recordResult to return an error can reinstate the link.
func TestPersistFailure_PersistJobsLockedReturnsErrAndNilFunc(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	withFailingMarshal(t)

	s.mu.Lock()
	save, err := s.persistJobsLocked()
	s.mu.Unlock()

	if save != nil {
		t.Fatal("save func should be nil on marshal failure")
	}
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("err = %v, want ErrPersistFailed", err)
	}
}
