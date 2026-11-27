package cron

// R51-QUAL-001 regression tests. persistLocked used to return a silent
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
	}, SchedulerDeps{})
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

// TestPersistFailure_MutationsReportIt: every mutation API surfaces
// ErrPersistFailed rather than a silent success that a restart would undo.
func TestPersistFailure_MutationsReportIt(t *testing.T) {
	prompt := "updated"
	for name, op := range map[string]func(s *Scheduler, id string) error{
		"AddJob": func(s *Scheduler, _ string) error {
			return s.AddJob(&Job{Schedule: "@every 1h", Prompt: "test", Platform: "feishu", ChatID: "chat1", ChatType: "direct", Paused: true})
		},
		"DeleteJobByID": func(s *Scheduler, id string) error { _, err := s.DeleteJobByID(id); return err },
		"ResumeJobByID": func(s *Scheduler, id string) error { _, err := s.ResumeJobByID(id); return err },
		"UpdateJob":     func(s *Scheduler, id string) error { _, err := s.UpdateJob(id, JobUpdate{Prompt: &prompt}); return err },
		"SetJobPrompt": func(s *Scheduler, _ string) error {
			// The dashboard placeholder: empty prompt, paused; AddJob refuses it.
			s.putJobForTest(&Job{ID: "abcd1234", Schedule: "@every 1h", Platform: "feishu", ChatID: "chat1", ChatType: "direct", Paused: true})
			return s.SetJobPrompt("abcd1234", "filled in")
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, id := newTestSchedulerForPersist(t)
			withFailingMarshal(t, s)
			if err := op(s, id); !errors.Is(err, ErrPersistFailed) {
				t.Fatalf("err = %v, want ErrPersistFailed", err)
			}
		})
	}
	t.Run("PauseJobByID", func(t *testing.T) {
		s, id := newTestSchedulerForPersist(t)
		if _, err := s.ResumeJobByID(id); err != nil {
			t.Fatal(err)
		}
		withFailingMarshal(t, s)
		if _, err := s.PauseJobByID(id); !errors.Is(err, ErrPersistFailed) {
			t.Fatalf("err = %v, want ErrPersistFailed", err)
		}
	})
}

// TestPersistFailure_RecordResultRollsBack verifies RNEW-011: when
// persistLocked fails inside recordResultP0WithSanitised, the
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
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Seed a job with concrete prior-run values so rollback is observable.
	j := &Job{
		ID:                  "abcd1234",
		Schedule:            "@every 1h",
		Platform:            "feishu",
		ChatID:              "chat1",
		ChatType:            "direct",
		Paused:              true,
		LastRunAt:           time.Unix(1000, 0),
		LastResult:          "prior-result",
		LastError:           "",
		LastSessionID:       "prior-sess",
		ConsecutiveFailures: 2,
	}
	s.putJobForTest(j)

	withFailingMarshal(t, s)

	// R230C-CR-1 / R232-ARCH-2 / R247-CR-14 (#586): tests exercise the
	// production path (recordTerminalResult, formerly recordResultP0WithSanitised)
	// directly. The discriminating fields (LastErrorClass, RunCounters) get
	// rolled back the same way as the four
	// LastRunAt/LastResult/LastError/LastSessionID fields.
	_, _, _ = s.recordTerminalResult(j.ID, runOutcome{result: "new-result", errMsg: "new-error", sessionID: "new-sess", errClass: ErrClassSessionError, state: RunStateFailed}, time.Now())

	snap := s.jobForTest(t, j.ID)

	if !snap.LastRunAt.Equal(time.Unix(1000, 0)) {
		t.Errorf("LastRunAt not reverted: got %v, want %v", snap.LastRunAt, time.Unix(1000, 0))
	}
	if snap.LastResult != "prior-result" {
		t.Errorf("LastResult not reverted: got %q, want %q", snap.LastResult, "prior-result")
	}
	if snap.LastError != "" {
		t.Errorf("LastError not reverted: got %q, want empty", snap.LastError)
	}
	if snap.LastSessionID != "prior-sess" {
		t.Errorf("LastSessionID not reverted: got %q, want %q", snap.LastSessionID, "prior-sess")
	}
	if snap.ConsecutiveFailures != 2 {
		t.Errorf("failure streak not reverted: got %d, want 2", snap.ConsecutiveFailures)
	}
}
