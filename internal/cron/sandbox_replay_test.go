package cron

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// replaySetup builds a sandbox scheduler with a side-effecting job and a
// persisted input snapshot for one original run, so ReplaySandboxRun has a
// payload to re-inject.
func replaySetup(t *testing.T, runner *fakeSandboxRunner) (*Scheduler, *recordingBroadcaster, *Job, string) {
	t.Helper()
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)
	origRunID := "feedfacefeedface"
	s.writeSandboxSnapshot(j.ID, origRunID, "replay this prompt", "haiku", "img-v1", nil, slog.Default())
	return s, rec, j, origRunID
}

// TestReplay_HappyPath: replaying a run with a snapshot dispatches a fresh
// sandbox run carrying the snapshot's prompt and replay_of=origRunID.
func TestReplay_HappyPath(t *testing.T) {
	runner := &fakeSandboxRunner{
		lines:   []string{`{"kind":"cli","line":{"type":"result","is_error":false,"result":"ok"}}`},
		outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "ok"},
	}
	s, rec, j, origRunID := replaySetup(t, runner)

	newRunID, err := s.ReplaySandboxRun(j.ID, origRunID)
	if err != nil {
		t.Fatalf("ReplaySandboxRun: %v", err)
	}
	if newRunID == "" || newRunID == origRunID {
		t.Fatalf("new run id = %q, want a fresh id distinct from %q", newRunID, origRunID)
	}
	waitEnded(t, rec)

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.gotJobs) != 1 {
		t.Fatalf("runner saw %d jobs, want 1 replay invocation", len(runner.gotJobs))
	}
	if runner.gotJobs[0].Prompt != "replay this prompt" {
		t.Errorf("replay injected prompt = %q, want the snapshot's prompt", runner.gotJobs[0].Prompt)
	}
	if runner.gotJobs[0].RunID != newRunID {
		t.Errorf("replay run id = %q, want %q", runner.gotJobs[0].RunID, newRunID)
	}
}

// TestReplay_StopsBeforeReplayWhenQueued: a queued (transport-failed) run is
// Stopped FIRST (§6.2 rule 1) before the replay dispatches.
func TestReplay_StopsBeforeReplayWhenQueued(t *testing.T) {
	runner := &fakeSandboxRunner{
		lines:   []string{`{"kind":"cli","line":{"type":"result","is_error":false,"result":"ok"}}`},
		outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "ok"},
	}
	s, rec, j, origRunID := replaySetup(t, runner)
	// Enqueue the original as a transport failure with a known runtime session.
	s.writeSandboxAttention(sandboxAttention{
		JobID: j.ID, RunID: origRunID,
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		Reason:           attentionReasonTransport, CreatedAtMS: time.Now().UnixMilli(),
	}, slog.Default())

	if _, err := s.ReplaySandboxRun(j.ID, origRunID); err != nil {
		t.Fatalf("ReplaySandboxRun: %v", err)
	}
	waitEnded(t, rec)

	runner.mu.Lock()
	stopped := append([]string(nil), runner.stopped...)
	runner.mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "run-feedfacefeedface-1234567890123456789" {
		t.Fatalf("§6.2 rule 1 violated: pre-replay Stop calls = %v, want the queued runtime id", stopped)
	}
	// The queue entry must be resolved after a successful replay.
	if s.SandboxAttentionCount() != 0 {
		t.Errorf("replay must resolve the queue entry; count = %d", s.SandboxAttentionCount())
	}
}

// TestReplay_RefusesWhenStopUnconfirmed pins the §6.2 safety: if the pre-replay
// Stop fails, replay is refused (ErrStopUnconfirmed) and NO new run dispatches.
func TestReplay_RefusesWhenStopUnconfirmed(t *testing.T) {
	runner := &fakeSandboxRunner{stopErr: errors.New("platform unreachable")}
	s, _, j, origRunID := replaySetup(t, runner)
	s.writeSandboxAttention(sandboxAttention{
		JobID: j.ID, RunID: origRunID,
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		Reason:           attentionReasonTransport, CreatedAtMS: time.Now().UnixMilli(),
	}, slog.Default())

	_, err := s.ReplaySandboxRun(j.ID, origRunID)
	if !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("err = %v, want ErrStopUnconfirmed", err)
	}
	runner.mu.Lock()
	n := len(runner.gotJobs)
	runner.mu.Unlock()
	if n != 0 {
		t.Fatalf("no replay run may dispatch when Stop is unconfirmed; runner saw %d jobs", n)
	}
	// The queue entry must remain (the incident is unresolved).
	if s.SandboxAttentionCount() != 1 {
		t.Errorf("queue entry must survive a refused replay; count = %d", s.SandboxAttentionCount())
	}
}

// TestReplay_NoSnapshot: replaying a run with no snapshot fails ErrNoSnapshot.
func TestReplay_NoSnapshot(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	j := sideEffectsJob(t, s)

	_, err := s.ReplaySandboxRun(j.ID, "abcabcabcabcabcd")
	if !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("err = %v, want ErrNoSnapshot", err)
	}
}

// TestReplay_JobNotFound: a missing job is rejected.
func TestReplay_JobNotFound(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	_, err := s.ReplaySandboxRun("0123456789abcdef", "feedfacefeedface")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}

// TestReplay_NonSandboxJob: a local-placement job cannot be replayed.
func TestReplay_NonSandboxJob(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	j := NewJobFull(JobInit{
		Schedule: "@daily", Prompt: "local job",
		IM:        JobIMContext{Platform: "dashboard", ChatID: "global"},
		Placement: PlacementLocal,
	})
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	_, err := s.ReplaySandboxRun(j.ID, "feedfacefeedface")
	if !errors.Is(err, ErrJobNotSandbox) {
		t.Fatalf("err = %v, want ErrJobNotSandbox", err)
	}
}

// TestReplay_InvalidID: a non-hex id is rejected before any work.
func TestReplay_InvalidID(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	if _, err := s.ReplaySandboxRun("../bad", "feedfacefeedface"); !errors.Is(err, errInvalidAttentionID) {
		t.Fatalf("err = %v, want errInvalidAttentionID", err)
	}
}

// TestReplay_NewRunCarriesReplayOf: the replayed run's persisted CronRun record
// links back via ReplayOf (the chain the dashboard renders).
func TestReplay_NewRunCarriesReplayOf(t *testing.T) {
	runner := &fakeSandboxRunner{
		lines:   []string{`{"kind":"cli","line":{"type":"result","is_error":false,"result":"ok"}}`},
		outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "ok"},
	}
	s, rec, j, origRunID := replaySetup(t, runner)

	newRunID, err := s.ReplaySandboxRun(j.ID, origRunID)
	if err != nil {
		t.Fatalf("ReplaySandboxRun: %v", err)
	}
	waitEnded(t, rec)

	run, err := s.Run(j.ID, newRunID)
	if err != nil {
		t.Fatalf("Run(%q): %v", newRunID, err)
	}
	if run.ReplayOf != origRunID {
		t.Errorf("new run ReplayOf = %q, want %q", run.ReplayOf, origRunID)
	}
}
