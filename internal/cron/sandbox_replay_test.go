package cron

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/runtelemetry"
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

// TestReplay_CorruptAttentionFailsClosed pins review PR-6 H1: a corrupt /
// unreadable attention record must REFUSE the replay (fail-closed), never fall
// through and skip the §6.2 rule-1 Stop. Without this, a torn write (the queue
// uses a plain WriteFile) would let a side-effecting run double-run.
func TestReplay_CorruptAttentionFailsClosed(t *testing.T) {
	runner := &fakeSandboxRunner{
		lines:   []string{`{"kind":"cli","line":{"type":"result","is_error":false,"result":"ok"}}`},
		outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "ok"},
	}
	s, _, j, origRunID := replaySetup(t, runner)
	// Stage a corrupt attention file for the original run (truncated JSON).
	dir := s.sandboxAttentionDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, origRunID+".json"), []byte("{not valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := s.ReplaySandboxRun(j.ID, origRunID)
	if !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("err = %v, want ErrStopUnconfirmed (corrupt record must fail closed)", err)
	}
	runner.mu.Lock()
	n := len(runner.gotJobs)
	stops := len(runner.stopped)
	runner.mu.Unlock()
	if n != 0 {
		t.Fatalf("no replay run may dispatch on a corrupt attention read; runner saw %d jobs", n)
	}
	_ = stops // Stop may or may not have been attempted; the invariant is "no dispatch".
}

// panicReplayRunner panics inside RunJob, simulating an executeSandbox panic
// that strikes AFTER the synchronous emitRunStarted but BEFORE the run reaches
// finishSandboxRun → emitRunEnded.
type panicReplayRunner struct{ stopped []string }

func (p *panicReplayRunner) RunJob(context.Context, SandboxJob, func([]byte) error) (SandboxOutcome, error) {
	panic("boom inside sandbox run")
}

func (p *panicReplayRunner) StopSession(_ context.Context, id string) error {
	p.stopped = append(p.stopped, id)
	return nil
}

// TestReplay_PanicStillEmitsRunEnded pins #2064: dispatchReplay fires
// emitRunStarted synchronously in the caller frame, then spawns the run on a
// goroutine. If the goroutine panics before reaching finishSandboxRun, the
// recover block must still emit a paired RunEnded — otherwise subscribers see
// a cron_run_started(queued) frame with no matching ended frame and the run
// hangs in "queued" forever.
func TestReplay_PanicStillEmitsRunEnded(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &panicReplayRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)
	origRunID := "feedfacefeedface"
	s.writeSandboxSnapshot(j.ID, origRunID, "replay this prompt", "haiku", "img-v1", nil, slog.Default())

	newRunID, err := s.ReplaySandboxRun(j.ID, origRunID)
	if err != nil {
		t.Fatalf("ReplaySandboxRun: %v", err)
	}

	// The started frame fired synchronously; the ended frame must follow even
	// though the run goroutine panicked.
	waitEnded(t, rec)

	if got := rec.startedCount(); got != 1 {
		t.Fatalf("RunStarted count = %d, want 1", got)
	}
	ev := rec.endedAtCron(0)
	if ev.RunID != newRunID {
		t.Fatalf("ended run id = %q, want the replay run id %q", ev.RunID, newRunID)
	}
	if ev.State == RunStateSucceeded {
		t.Fatalf("panicked run must not report succeeded; state = %q", ev.State)
	}
	if ev.StartedAt.IsZero() {
		t.Fatal("ended frame must carry the original StartedAt so the timeline pairs")
	}
}

// finalizeOrderBroadcaster records, at the instant BroadcastRunEnded fires,
// whether CurrentRun(jobID) has already been finalized (ok=false). It pins the
// finalize-before-broadcast contract (R246-GO-3 / #689) for the replay panic
// path (#2094): if emitRunEnded fired before finalize, the probe would observe
// the run still inflight (ok=true).
type finalizeOrderBroadcaster struct {
	recordingBroadcaster
	probe func(jobID string) (RunInflightView, bool)
	mu    sync.Mutex
	// inflightAtBroadcast is true if any ended-broadcast observed the run
	// still inflight (contract violation).
	inflightAtBroadcast bool
}

func (b *finalizeOrderBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	if b.probe != nil {
		if _, ok := b.probe(ev.OwnerID); ok {
			b.mu.Lock()
			b.inflightAtBroadcast = true
			b.mu.Unlock()
		}
	}
	b.recordingBroadcaster.BroadcastRunEnded(ev)
}

func (b *finalizeOrderBroadcaster) observedInflight() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inflightAtBroadcast
}

// TestReplay_PanicFinalizesBeforeBroadcast pins #2094: on the panic-recover
// path of dispatchReplay, finalizer.finalize() must run before emitRunEnded so
// a concurrent dashboard list observing the cron_run_ended frame sees
// CurrentRun(jobID) == ok:false (run already released), not a stale inflight
// view. Before the fix, defer LIFO ran emitRunEnded (recover defer) before the
// outer finalize defer, leaving the run momentarily inflight when the ended
// frame broadcast.
func TestReplay_PanicFinalizesBeforeBroadcast(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &panicReplayRunner{}
	rec := &finalizeOrderBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath},
		SchedulerDeps{Router: panicRouter{t: t}, Telemetry: rec, Sandbox: runner})
	t.Cleanup(func() { s.Stop() })
	rec.probe = s.CurrentRun

	j := sideEffectsJob(t, s)
	origRunID := "feedfacefeedface"
	s.writeSandboxSnapshot(j.ID, origRunID, "replay this prompt", "haiku", "img-v1", nil, slog.Default())

	if _, err := s.ReplaySandboxRun(j.ID, origRunID); err != nil {
		t.Fatalf("ReplaySandboxRun: %v", err)
	}
	waitEnded(t, &rec.recordingBroadcaster)

	if rec.observedInflight() {
		t.Fatal("emitRunEnded broadcast before finalize: CurrentRun still reported the run inflight when the ended frame fired (finalize-before-broadcast contract violated, #2094)")
	}
	// And the run must genuinely be released afterwards (no leak).
	if _, ok := s.CurrentRun(j.ID); ok {
		t.Fatal("run still inflight after panic-recover finalize; gate leaked")
	}
}

// TestReplay_AfterStopRejected pins review PR-6 H2: ReplaySandboxRun after the
// scheduler is stopped returns ErrSchedulerStopped (Add-before-Wait safety) and
// dispatches nothing.
func TestReplay_AfterStopRejected(t *testing.T) {
	runner := &fakeSandboxRunner{
		lines:   []string{`{"kind":"cli","line":{"type":"result","is_error":false,"result":"ok"}}`},
		outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "ok"},
	}
	s, _, j, origRunID := replaySetup(t, runner)
	s.Stop() // sets s.stopped + drains triggerWG

	_, err := s.ReplaySandboxRun(j.ID, origRunID)
	if !errors.Is(err, ErrSchedulerStopped) {
		t.Fatalf("err = %v, want ErrSchedulerStopped after Stop()", err)
	}
	runner.mu.Lock()
	n := len(runner.gotJobs)
	runner.mu.Unlock()
	if n != 0 {
		t.Fatalf("no replay may dispatch after Stop; runner saw %d jobs", n)
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

// TestReplay_PanicKeepsStartedEndedBalanced pins R202606-ARCH-2: when the
// replay goroutine panics before reaching finishSandboxRun/recordTerminalResult,
// the panic-recover path must bump CronRunEndedTotal to match the
// CronRunStartedTotal that emitRunStarted already bumped synchronously.
// Without the fix (metrics.CronRunEndedTotal.Add(1) in the recover block),
// Started and Ended diverge by 1 permanently, breaking the in-flight gauge
// invariant.
func TestReplay_PanicKeepsStartedEndedBalanced(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &panicReplayRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sideEffectsJob(t, s)
	origRunID := "feedfacefeedface"
	s.writeSandboxSnapshot(j.ID, origRunID, "panic metrics test", "haiku", "img-v1", nil, slog.Default())

	startedBefore := metrics.CronRunStartedTotal.Value()
	endedBefore := metrics.CronRunEndedTotal.Value()

	if _, err := s.ReplaySandboxRun(j.ID, origRunID); err != nil {
		t.Fatalf("ReplaySandboxRun: %v", err)
	}
	waitEnded(t, rec)

	startedDelta := metrics.CronRunStartedTotal.Value() - startedBefore
	endedDelta := metrics.CronRunEndedTotal.Value() - endedBefore
	if startedDelta != 1 {
		t.Fatalf("CronRunStartedTotal delta = %d, want 1 [R202606-ARCH-2]", startedDelta)
	}
	if endedDelta != 1 {
		t.Fatalf("CronRunEndedTotal delta = %d, want 1 — panic path must bump CronRunEndedTotal to keep started/ended balanced [R202606-ARCH-2]", endedDelta)
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
