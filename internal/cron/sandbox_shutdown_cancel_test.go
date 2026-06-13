package cron

import (
	"context"
	"path/filepath"
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

// TestSandbox_ShutdownCancelSideEffectingDoesNotEnqueueAttention covers
// R20260613-LB-1 (#2081): a side_effects=true sandbox run that is canceled ONLY
// by graceful shutdown (ctx.Err()==context.Canceled, classified RunStateCanceled
// by #2059) must NOT be written to the §7.4 human-confirmation queue. Before the
// fix the attention write ran unconditionally above the cancel switch, so a
// plain restart left the operator a phantom "needs confirm" entry that
// reconcileSandboxPending would later overwrite with reason=orphaned — for a run
// whose history correctly reads Canceled.
func TestSandbox_ShutdownCancelSideEffectingDoesNotEnqueueAttention(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	runner := &blockingSandboxRunner{entered: make(chan struct{})}
	rec := &recordingBroadcaster{}
	s := NewScheduler(
		SchedulerConfig{MaxJobs: 5, ParentCtx: parent, StorePath: filepath.Join(t.TempDir(), "cron_jobs.json")},
		SchedulerDeps{Router: panicRouter{t: t}, Telemetry: rec, Sandbox: runner},
	)
	t.Cleanup(func() { s.Stop() })

	j := sideEffectsJob(t, s)

	done := make(chan struct{})
	go func() {
		s.executeOpt(j, true)
		close(done)
	}()

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

	if ev := rec.endedAtCron(0); ev.State != RunStateCanceled {
		t.Fatalf("state = %q, want canceled", ev.State)
	}
	// The core assertion: shutdown-cancel must keep the run out of the queue.
	if n := s.SandboxAttentionCount(); n != 0 {
		t.Fatalf("queue count = %d, want 0 (shutdown-cancel must not enqueue attention even with side_effects)", n)
	}
	if items := s.ListSandboxAttention(); len(items) != 0 {
		t.Fatalf("queue len = %d, want 0", len(items))
	}
}

// TestSandbox_DeadlineExceededSideEffectingEnqueuesAttention is the positive
// control for #2081: a genuine transport failure where the run ctx hits its own
// ExecTimeout deadline (ctx.Err()==context.DeadlineExceeded, NOT a parent
// shutdown cancel) on a side-effecting job MUST still enter the confirmation
// queue.
func TestSandbox_DeadlineExceededSideEffectingEnqueuesAttention(t *testing.T) {
	runner := &blockingSandboxRunner{entered: make(chan struct{})}
	rec := &recordingBroadcaster{}
	s := NewScheduler(
		// Tiny ExecTimeout: the run ctx = WithTimeout(stopCtx, budget) expires on
		// its own, so the runner (blocked on ctx.Done()) observes
		// ctx.Err()==DeadlineExceeded — a real transport timeout, not a shutdown.
		SchedulerConfig{MaxJobs: 5, ExecTimeout: 20 * time.Millisecond, StorePath: filepath.Join(t.TempDir(), "cron_jobs.json")},
		SchedulerDeps{Router: panicRouter{t: t}, Telemetry: rec, Sandbox: runner},
	)
	t.Cleanup(func() { s.Stop() })

	j := sideEffectsJob(t, s)

	done := make(chan struct{})
	go func() {
		s.executeOpt(j, true)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeOpt did not return")
	}
	waitEnded(t, rec)

	if ev := rec.endedAtCron(0); ev.State != RunStateTimedOut {
		t.Fatalf("state = %q, want timed_out", ev.State)
	}
	if n := s.SandboxAttentionCount(); n != 1 {
		t.Fatalf("queue count = %d, want 1 (real DeadlineExceeded transport failure on side-effecting job must enqueue)", n)
	}
}
