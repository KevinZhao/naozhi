package cron

import (
	"context"
	"testing"
	"time"
)

// blockingSandboxRunner blocks inside RunJob until the run ctx is canceled,
// then returns a transport failure with ctx.Err()'s flavor — exactly what a
// real runner reports when the scheduler's stopCtx is canceled mid-invoke.
type blockingSandboxRunner struct {
	entered chan struct{} // closed once RunJob is in flight
}

func (b *blockingSandboxRunner) StopSession(context.Context, string) error { return nil }

func (b *blockingSandboxRunner) RunJob(ctx context.Context, _ SandboxJob, _ func([]byte) error) (SandboxOutcome, error) {
	close(b.entered)
	<-ctx.Done()
	return SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: ctx.Err().Error()}, nil
}

// TestSandbox_ShutdownCancelMapsToCanceled covers R20260613-CR-6 (#2059): when
// the scheduler is shutting down (stopCtx canceled), an in-flight sandbox run
// observes ctx.Err()==context.Canceled. That must be classified as
// RunStateCanceled (not RunStateFailed/sandbox_transport) and must NOT persist
// a failure record — mirroring the local path (scheduler_run.go), which uses
// RunStateCanceled + skipPersist for shutdown-cancel.
func TestSandbox_ShutdownCancelMapsToCanceled(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	runner := &blockingSandboxRunner{entered: make(chan struct{})}
	rec := &recordingBroadcaster{}
	s := NewScheduler(
		SchedulerConfig{MaxJobs: 5, ParentCtx: parent},
		SchedulerDeps{Router: panicRouter{t: t}, Telemetry: rec, Sandbox: runner},
	)
	t.Cleanup(func() { s.Stop() })

	j := sandboxJob(t, s)

	done := make(chan struct{})
	go func() {
		s.executeOpt(j, true)
		close(done)
	}()

	// Wait until the runner is mid-invoke, then cancel the parent ctx (the
	// stopCtx derives from it) to simulate scheduler shutdown.
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never entered RunJob")
	}
	cancelParent()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeOpt did not return after shutdown-cancel")
	}
	waitEnded(t, rec)

	ev := rec.endedAtCron(0)
	if ev.State != RunStateCanceled {
		t.Fatalf("state = %q, want canceled", ev.State)
	}
	if ev.ErrorClass != ErrClassCanceled {
		t.Fatalf("error_class = %q, want canceled", ev.ErrorClass)
	}
	// skipPersist semantics: a shutdown-canceled run must not stamp LastRunAt
	// / LastResult onto the persisted job (history stays clean), matching the
	// local fresh-spawn shutdown path.
	jobs := s.ListJobs("dashboard", "global")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if !jobs[0].LastRunAt.IsZero() {
		t.Fatalf("LastRunAt = %v, want zero (skipPersist on shutdown-cancel)", jobs[0].LastRunAt)
	}
}
