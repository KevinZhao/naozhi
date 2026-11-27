package cron

import (
	"context"
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

// TestP0_FinishRunCanceledSkipsPersist: ctx-canceled paths set
// skipPersist=true so LastRunAt is preserved (matches historical
// behaviour). state counter still bumps.
func TestP0_FinishRunCanceledSkipsPersist(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}, Telemetry: rec})
	prevRun := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	j := &Job{ID: "job-c", Schedule: "@every 5m", LastRunAt: prevRun}
	s.putJobForTest(j)

	s.finishRun(runCtx{jobID: j.ID, runID: "r1", startedAt: time.Now(), trigger: TriggerScheduled}, runOutcome{state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: context.Canceled.Error(), skipPersist: true})

	if got := s.jobForTest(t, j.ID); !got.LastRunAt.Equal(prevRun) {
		t.Errorf("skipPersist=true must leave LastRunAt unchanged: got %v want %v", got.LastRunAt, prevRun)
	}
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
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-w", Schedule: "@every 5m", FreshContext: true, WorkDir: "/nonexistent-naozhi-test-dir-xyz"}
	s.putJobForTest(j)

	snap := jobSnapshot{
		jobID: "job-w", schedule: "@every 5m", workDir: j.WorkDir, fresh: true,
	}
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	stubRefresh, ok := s.freshContextPreflightP0(preflightArgs{runCtx: runCtx{
		jobID: j.ID, snap: snap, key: sessionkey.CronKey(j.ID), lg: lg,
		notifyTo: NotifyTarget{}, runID: "r1", startedAt: time.Now(), trigger: TriggerScheduled,
	}})
	if ok {
		t.Fatal("preflight should bail when workdir unreachable")
	}
	stubRefresh.run()
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
