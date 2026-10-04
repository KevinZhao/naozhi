package cron

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// panickingSandboxRunner panics inside RunJob, before the sandbox path
// reaches its own finish.
type panickingSandboxRunner struct{}

func (panickingSandboxRunner) StopSession(context.Context, string) error { return nil }
func (panickingSandboxRunner) RunJob(context.Context, SandboxJob, func([]byte) error) (SandboxOutcome, error) {
	panic("sandbox sdk")
}

// A sandbox run whose finish panics in its persistence half has already been
// ended for subscribers; the scaffold's recover must find the same run guard
// claimed instead of closing the run a second time as failed (#3102). Not
// parallel: the counters are process-wide.
func TestSandboxRunPanicInsideFinish_EndsOnce(t *testing.T) {
	endedBase := metrics.CronRunEndedTotal.Value()
	failedBase := metrics.CronRunFailedTotal.Value()
	sandboxFailedBase := metrics.CronSandboxRunFailedTotal.Value()
	runner := &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "ok"}}
	s, rec := sandboxTestScheduler(t, runner, filepath.Join(t.TempDir(), "cron_jobs.json"))
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// Panic once: a second finish, if one happens, must run to completion so
	// the test observes it rather than a panic escaping executeOpt.
	var hookCalls atomic.Int32
	s.finishRunPreAppendHook = func(string) {
		if hookCalls.Add(1) == 1 {
			panic("persist half")
		}
	}
	j := sandboxJob(t, s)

	s.executeOpt(j.ID, true)

	if rec.endedCount() != 1 {
		t.Fatalf("ended = %d, want exactly one", rec.endedCount())
	}
	if ev := rec.endedAtCron(0); ev.State != RunStateSucceeded || ev.ErrorClass != ErrClassNone {
		t.Errorf("ended = %s/%q, want the run's own succeeded (the recover must not re-close it)", ev.State, ev.ErrorClass)
	}
	if d := metrics.CronRunEndedTotal.Value() - endedBase; d != 1 {
		t.Errorf("CronRunEndedTotal moved %d, want 1", d)
	}
	if d := metrics.CronRunFailedTotal.Value() - failedBase; d != 0 {
		t.Errorf("CronRunFailedTotal moved %d, want 0", d)
	}
	if d := metrics.CronSandboxRunFailedTotal.Value() - sandboxFailedBase; d != 0 {
		t.Errorf("CronSandboxRunFailedTotal moved %d, want 0", d)
	}
	got, ok := s.GetJob(j.ID)
	if !ok {
		t.Fatal("job vanished")
	}
	if c := got.RunCounters; c.Total != 1 || c.Succeeded != 1 || c.Failed != 0 {
		t.Errorf("RunCounters = %+v, want one succeeded run", c)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty: the succeeded run was overwritten by a second finish", got.LastError)
	}
	if _, running := s.CurrentRun(j.ID); running {
		t.Error("the job still shows a run in flight")
	}
}

// A sandbox-placement run that panics before its own finish is closed by the
// scaffold as a sandbox run, so the sandbox failure bucket stays a subset of
// the run totals (#2173, #3102). Not parallel: the counters are process-wide.
func TestSandboxRunPanicBeforeFinish_ClosedAsSandbox(t *testing.T) {
	endedBase := metrics.CronRunEndedTotal.Value()
	sandboxFailedBase := metrics.CronSandboxRunFailedTotal.Value()
	inflightBase := metrics.CronRunInflight.Value()
	s, rec := sandboxTestScheduler(t, panickingSandboxRunner{}, filepath.Join(t.TempDir(), "cron_jobs.json"))
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	j := sandboxJob(t, s)

	s.executeOpt(j.ID, true)

	if rec.endedCount() != 1 {
		t.Fatalf("ended = %d, want exactly one", rec.endedCount())
	}
	if ev := rec.endedAtCron(0); ev.State != RunStateFailed || ev.ErrorClass != ErrClassPanic {
		t.Errorf("ended = %s/%s, want failed/panic", ev.State, ev.ErrorClass)
	}
	if d := metrics.CronRunEndedTotal.Value() - endedBase; d != 1 {
		t.Errorf("CronRunEndedTotal moved %d, want 1", d)
	}
	if d := metrics.CronSandboxRunFailedTotal.Value() - sandboxFailedBase; d != 1 {
		t.Errorf("CronSandboxRunFailedTotal moved %d, want 1", d)
	}
	if metrics.CronRunInflight.Value() != inflightBase {
		t.Errorf("inflight gauge = %d, want %d", metrics.CronRunInflight.Value(), inflightBase)
	}
	testhelper.Eventually(t, func() bool { return len(s.ListRuns(j.ID, 10, time.Time{})) == 1 },
		time.Second, "the panicked run has no history record")
	if _, running := s.CurrentRun(j.ID); running {
		t.Error("the job still shows a run in flight")
	}
}
