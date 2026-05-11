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
	"sync/atomic"
	"testing"
	"time"
)

// withFailingMarshal swaps marshalJobs to a stub that always errors, then
// restores the original on test cleanup. Centralised so each mutation case
// stays focused on its assertion and new callers inherit the same cleanup.
//
// Uses atomic.Pointer.Swap so parallel tests in the same package do not race
// on the function-value word.
func withFailingMarshal(t *testing.T) {
	t.Helper()
	failing := marshalJobsFn(func(any) ([]byte, error) {
		return nil, fmt.Errorf("injected marshal failure")
	})
	orig := marshalJobs.Swap(&failing)
	t.Cleanup(func() { marshalJobs.Store(orig) })
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

// TestPersistFailure_RecordResultRollsBack verifies RNEW-011: when
// persistJobsLocked fails inside recordResult, the four in-memory fields
// (LastRunAt / LastResult / LastError / LastSessionID) must revert to their
// pre-mutation values so the live WS broadcast and the on-disk snapshot stay
// in sync. Before the fix, the fields were overwritten and kept even when
// disk write failed, causing dashboard → JSONL divergence across restarts.
//
// The test also asserts onExecute is NOT called on the failure path: the
// broadcast would otherwise promote the not-persisted result to dashboard
// viewers, which is exactly the divergence the rollback prevents.
func TestPersistFailure_RecordResultRollsBack(t *testing.T) {
	dir := t.TempDir()
	var broadcasts int32
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	s.SetOnExecute(func(id, result, errMsg string) {
		atomic.AddInt32(&broadcasts, 1)
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Seed a job with concrete prior-run values so rollback is observable.
	j := &Job{
		ID:            "abcd1234",
		Schedule:      "@every 1h",
		Platform:      "feishu",
		ChatID:        "chat1",
		ChatType:      "direct",
		Paused:        true,
		LastRunAt:     time.Unix(1000, 0),
		LastResult:    "prior-result",
		LastError:     "",
		LastSessionID: "prior-sess",
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	withFailingMarshal(t)

	s.recordResult(j, "new-result", "new-error", "new-sess")

	s.mu.Lock()
	defer s.mu.Unlock()

	if !j.LastRunAt.Equal(time.Unix(1000, 0)) {
		t.Errorf("LastRunAt not reverted: got %v, want %v", j.LastRunAt, time.Unix(1000, 0))
	}
	if j.LastResult != "prior-result" {
		t.Errorf("LastResult not reverted: got %q, want %q", j.LastResult, "prior-result")
	}
	if j.LastError != "" {
		t.Errorf("LastError not reverted: got %q, want empty", j.LastError)
	}
	if j.LastSessionID != "prior-sess" {
		t.Errorf("LastSessionID not reverted: got %q, want %q", j.LastSessionID, "prior-sess")
	}
	if got := atomic.LoadInt32(&broadcasts); got != 0 {
		t.Errorf("onExecute fired %d times on persist failure; expected 0 so dashboard doesn't promote un-persisted result", got)
	}
}

// TestPersistFailure_RecordResultHappyPathBroadcasts is the positive
// counterpart: when persistJobsLocked succeeds, recordResult must apply the
// new values AND invoke onExecute so dashboard clients see the live update.
// Without this counterpart a regression that accidentally always rolls back
// (e.g. inverted error check) would pass the rollback test above.
func TestPersistFailure_RecordResultHappyPathBroadcasts(t *testing.T) {
	dir := t.TempDir()
	var broadcasts int32
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	s.SetOnExecute(func(id, result, errMsg string) {
		atomic.AddInt32(&broadcasts, 1)
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	j := &Job{
		ID:         "abcd5678",
		Schedule:   "@every 1h",
		Platform:   "feishu",
		ChatID:     "chat1",
		ChatType:   "direct",
		Paused:     true,
		LastResult: "prior",
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Real marshaler — persist succeeds.
	s.recordResult(j, "fresh-result", "", "sess-1")

	s.mu.Lock()
	defer s.mu.Unlock()

	if j.LastResult != "fresh-result" {
		t.Errorf("LastResult not applied: got %q, want %q", j.LastResult, "fresh-result")
	}
	if j.LastSessionID != "sess-1" {
		t.Errorf("LastSessionID not applied: got %q, want %q", j.LastSessionID, "sess-1")
	}
	if got := atomic.LoadInt32(&broadcasts); got != 1 {
		t.Errorf("onExecute fired %d times on happy path; expected 1", got)
	}
}

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
