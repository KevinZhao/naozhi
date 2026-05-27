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
	"time"
)

// withFailingMarshal swaps the per-Scheduler marshalJobs field to a stub
// that always errors, then restores the original on test cleanup.
// Centralised so each mutation case stays focused on its assertion and
// new callers inherit the same cleanup.
//
// Uses atomic.Pointer.Swap so parallel tests in the same package do not race
// on the function-value word.
//
// R250-ARCH-14: takes a *Scheduler so the seam is per-instance, not a
// package global; this prevents one test's failing stub from leaking
// into a parallel scheduler instance via init/Cleanup ordering.
func withFailingMarshal(t *testing.T, s *Scheduler) {
	t.Helper()
	failing := marshalJobsFn(func(any) ([]byte, error) {
		return nil, fmt.Errorf("injected marshal failure")
	})
	orig := s.marshalJobs.Swap(&failing)
	t.Cleanup(func() { s.marshalJobs.Store(orig) })
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

	withFailingMarshal(t, s)

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

	withFailingMarshal(t, s)

	_, err := s.DeleteJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("DeleteJobByID err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_DeleteJobByPrefix(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	withFailingMarshal(t, s)

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

	withFailingMarshal(t, s)

	_, err := s.PauseJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJobByID err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_ResumeJobByID(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	// Seed starts Paused=true, so Resume is the natural mutation.

	withFailingMarshal(t, s)

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

	withFailingMarshal(t, s)

	_, err := s.PauseJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("PauseJob err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_ResumeJobByPrefix(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	withFailingMarshal(t, s)

	_, err := s.ResumeJob(id[:4], "feishu", "chat1")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJob err = %v, want ErrPersistFailed", err)
	}
}

func TestPersistFailure_UpdateJob(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	withFailingMarshal(t, s)

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

	withFailingMarshal(t, s)

	err := s.SetJobPrompt(j.ID, "filled in")
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("SetJobPrompt err = %v, want ErrPersistFailed", err)
	}

	// Rollback assertions: in-memory state must revert to the pre-call values
	// so that a process restart does not see a partially-applied mutation.
	s.mu.RLock()
	defer s.mu.RUnlock()
	if j.Prompt != "" {
		t.Errorf("rollback: j.Prompt = %q, want empty string", j.Prompt)
	}
	if !j.Paused {
		t.Errorf("rollback: j.Paused = false, want true (initial state)")
	}
}

// TestPersistFailure_RecordResultRollsBack verifies RNEW-011: when
// persistJobsLocked fails inside recordResultP0WithSanitised, the
// in-memory fields (LastRunAt / LastResult / LastError / LastSessionID
// / LastErrorClass / RunCounters) must revert to their
// pre-mutation values so the live WS broadcast and the on-disk snapshot stay
// in sync. Before the fix, the fields were overwritten and kept even when
// disk write failed, causing dashboard → JSONL divergence across restarts.
//
// Phase D (RFC §3.5) deleted the OnExecute hook; the rollback contract
// is now observed purely through Job field reverts (LastResult /
// LastError / LastSessionID stay at prior values). The dashboard never
// promotes an un-persisted result because cron_run_ended only fires from
// finishRun, which gates on jobPersistOK.
func TestPersistFailure_RecordResultRollsBack(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
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

	withFailingMarshal(t, s)

	// R230C-CR-1 / R232-ARCH-2: tests now exercise the production path
	// (recordResultP0WithSanitised) directly. The discriminating fields
	// (LastErrorClass, RunCounters) get rolled back the same way as the
	// four LastRunAt/LastResult/LastError/LastSessionID fields.
	_, _, _ = s.recordResultP0WithSanitised(j, "new-result", "new-error", "new-sess", ErrClassSessionError, RunStateFailed)

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
}

// TestPersistFailure_RecordResultHappyPathApplies is the positive
// counterpart: when persistJobsLocked succeeds, recordResultP0WithSanitised
// must apply the new Job values. Without this counterpart a regression
// that accidentally always rolls back (e.g. inverted error check) would
// pass the rollback test above.
//
// Phase D (RFC §3.5) deleted the OnExecute broadcast hook; the assertion
// now observes Job state alone — finishRun's runtelemetry path is
// covered by run_p0_test.go's RunEndedEvent assertions.
func TestPersistFailure_RecordResultHappyPathApplies(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
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

	// Real marshaler — persist succeeds. R230C-CR-1: exercises the
	// production path directly so a future change to recordResultP0
	// can't silently rot a happy-path test that exercises a separate
	// helper.
	_, _, _ = s.recordResultP0WithSanitised(j, "fresh-result", "", "sess-1", ErrClassNone, RunStateSucceeded)

	s.mu.Lock()
	defer s.mu.Unlock()

	if j.LastResult != "fresh-result" {
		t.Errorf("LastResult not applied: got %q, want %q", j.LastResult, "fresh-result")
	}
	if j.LastSessionID != "sess-1" {
		t.Errorf("LastSessionID not applied: got %q, want %q", j.LastSessionID, "sess-1")
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

	withFailingMarshal(t, s)

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
