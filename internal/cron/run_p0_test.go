package cron

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// fakeRouter is a minimal SessionRouter that returns a configurable error
// from GetOrCreate so executeOpt's failure branches can be exercised
// without spinning up a real CLI. RegisterCronStubWithChain is a no-op;
// Reset captures the key for assertions.
type fakeRouter struct {
	mu       sync.Mutex
	getErr   error
	resetKey string
}

func (f *fakeRouter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string) {
}
func (f *fakeRouter) Reset(key string) {
	f.mu.Lock()
	f.resetKey = key
	f.mu.Unlock()
}
func (f *fakeRouter) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error) {
	return nil, SessionExisting, f.getErr
}

// TestP0_OverlapSkippedEmitsTerminalEvent: when the CAS gate rejects a
// re-entrant execute, finishRun must still fire so the dashboard sees the
// skip. The callback shape is the contract dashboard.go relies on.
func TestP0_OverlapSkippedEmitsTerminalEvent(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, Telemetry: rec})

	// Manually trip the inflight gate as if a run were in flight.
	inf := s.jobInflight("job-overlap")
	if !inf.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	defer inf.running.Store(false)

	// Now simulate a TriggerNow racing the in-flight run. emitOverlapSkipped
	// is the synthetic terminal event for this path; it should record one
	// run-ended with state=skipped + class=overlap_skipped.
	j := &Job{ID: "job-overlap", Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.emitOverlapSkipped(j, true)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateSkipped {
		t.Errorf("state: want skipped, got %q", got.State)
	}
	if got.ErrorClass != ErrClassOverlapSkipped {
		t.Errorf("error_class: want overlap_skipped, got %q", got.ErrorClass)
	}
	if got.JobID != "job-overlap" {
		t.Errorf("job_id: want job-overlap, got %q", got.JobID)
	}
	if got.Trigger != TriggerManual {
		t.Errorf("trigger: want manual, got %q", got.Trigger)
	}
}

// TestP0_RunCountersAdvanceByState: Job.RunCounters bumps by terminal state.
// Mirrors the list API stats field.
func TestP0_RunCountersAdvanceByState(t *testing.T) {
	t.Parallel()
	c := JobRunCounters{}
	c.addRun(RunStateSucceeded)
	c.addRun(RunStateSucceeded)
	c.addRun(RunStateFailed)
	c.addRun(RunStateSkipped)
	c.addRun(RunStateTimedOut)
	c.addRun(RunStateCanceled)

	want := JobRunCounters{Total: 6, Succeeded: 2, Failed: 1, Skipped: 1, TimedOut: 1, Canceled: 1}
	if c != want {
		t.Errorf("counters: got %+v want %+v", c, want)
	}
}

// TestP0_InflightSnapshotReflectsCASState: snapshot returns ok=false when
// the CAS gate is open, true with populated metadata otherwise. Used by
// list API's CurrentRun path.
func TestP0_InflightSnapshotReflectsCASState(t *testing.T) {
	t.Parallel()
	inf := &runInflight{}
	if _, ok := inf.snapshot(); ok {
		t.Fatal("zero inflight should snapshot as not-running")
	}
	if !inf.running.CompareAndSwap(false, true) {
		t.Fatal("CAS")
	}
	// R238-ARCH-3 (#742): writers go through populate / setPhase /
	// setSessionID instead of touching per-field atomic.Pointers
	// directly; test exercises the same observable surface.
	st := time.Now()
	inf.populate(runInflightView{
		RunID:     "abc",
		StartedAt: st,
		Phase:     PhaseSending,
		Trigger:   TriggerScheduled,
	})
	v, ok := inf.snapshot()
	if !ok {
		t.Fatal("running snapshot should return ok=true")
	}
	if v.RunID != "abc" || v.Phase != PhaseSending || v.Trigger != TriggerScheduled {
		t.Errorf("snapshot fields: %+v", v)
	}
	inf.running.Store(false)
	if _, ok := inf.snapshot(); ok {
		t.Fatal("snapshot must follow CAS gate")
	}
}

// TestP0_FinishRunCanceledSkipsPersist: ctx-canceled paths set
// skipPersist=true so LastRunAt is preserved (matches historical
// behaviour). state counter still bumps.
func TestP0_FinishRunCanceledSkipsPersist(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, Telemetry: rec})
	prevRun := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	j := &Job{ID: "job-c", Schedule: "@every 5m", LastRunAt: prevRun}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.finishRun(finishArgs{
		job: j, runID: "r1", startedAt: time.Now(), trigger: TriggerScheduled,
		state: RunStateCanceled, errClass: ErrClassCanceled,
		errMsg: context.Canceled.Error(), skipPersist: true,
	})

	s.mu.RLock()
	if !j.LastRunAt.Equal(prevRun) {
		t.Errorf("skipPersist=true must leave LastRunAt unchanged: got %v want %v", j.LastRunAt, prevRun)
	}
	s.mu.RUnlock()
	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	endedSeen := rec.endedAtCron(0)
	if endedSeen.State != RunStateCanceled {
		t.Errorf("ended event state: %q", endedSeen.State)
	}
	if endedSeen.ErrorClass != ErrClassCanceled {
		t.Errorf("ended event error_class: %q", endedSeen.ErrorClass)
	}
}

// TestP0_PreflightWorkdirUnreachableMapsCorrectErrorClass: ensures the
// fresh-mode workdir-unreachable branch lands as failed/workdir_unreachable
// rather than the historical raw "work_dir unreachable" string.
func TestP0_PreflightWorkdirUnreachableMapsCorrectErrorClass(t *testing.T) {
	t.Parallel()
	router := &fakeRouter{}
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

	j := &Job{ID: "job-w", Schedule: "@every 5m", FreshContext: true, WorkDir: "/nonexistent-naozhi-test-dir-xyz"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	snap := jobSnapshot{
		jobID: "job-w", schedule: "@every 5m", workDir: j.WorkDir, fresh: true,
	}
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	stubRefresh, ok := s.freshContextPreflightP0(preflightArgs{
		job: j, snap: snap, key: sessionkey.CronKey(j.ID), lg: lg,
		notifyTo: NotifyTarget{}, runID: "r1", startedAt: time.Now(), trigger: TriggerScheduled,
	})
	if ok {
		t.Fatal("preflight should bail when workdir unreachable")
	}
	stubRefresh()
	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	endedSeen := rec.endedAtCron(0)
	if endedSeen.State != RunStateFailed {
		t.Errorf("state: %q", endedSeen.State)
	}
	if endedSeen.ErrorClass != ErrClassWorkDirUnreachable {
		t.Errorf("error_class: %q want %q", endedSeen.ErrorClass, ErrClassWorkDirUnreachable)
	}
	// Ensure the fakeRouter's Reset was NOT called: workdir guard fires
	// before Reset so the existing session is preserved for the next
	// successful run.
	router.mu.Lock()
	defer router.mu.Unlock()
	if router.resetKey != "" {
		t.Errorf("Reset should not fire when workdir unreachable, got key=%q", router.resetKey)
	}
}

// silence unused import errors when running -short selectively
var _ = errors.New
