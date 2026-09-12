package cron

import (
	"context"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// errGetSessionRouter hands back a GetOrCreate error so the executeGetSession
// session-error branch is exercised without a real CLI.
type errGetSessionRouter struct {
	reapRouter
	getErr error
}

func (r *errGetSessionRouter) GetOrCreate(_ context.Context, _ string, _ AgentOpts) (Session, SessionStatus, error) {
	return nil, SessionExisting, r.getErr
}

// cancelGetSessionRouter returns context.Canceled from GetOrCreate to exercise
// the executeGetSession cancel branch.
type cancelGetSessionRouter struct {
	reapRouter
}

func (r *cancelGetSessionRouter) GetOrCreate(_ context.Context, _ string, _ AgentOpts) (Session, SessionStatus, error) {
	return nil, SessionExisting, context.Canceled
}

// TestFreshGetSession_SessionError_ResetsBeforeFinishRun verifies that when
// fresh-mode GetOrCreate returns a non-cancel error, Reset(cronKey) is called
// (at least once for the preflight, and once for the error-path reap) before
// finishRun records the failure. Uses an errGetSessionRouter that records Reset
// calls so ordering is observable.
func TestFreshGetSession_SessionError_ResetsBeforeFinishRun(t *testing.T) {
	t.Parallel()

	ord := &orderRecorder{}
	rec := &recordingBroadcaster{order: ord}
	router := &errGetSessionRouter{
		reapRouter: reapRouter{order: ord},
		getErr:     errors.New("session backend unavailable"),
	}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-fresh-getsess-err", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow: skip jitter */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateFailed && got.State != RunStateSkipped {
		t.Fatalf("state: want failed/skipped on session error, got %q (errClass=%q)", got.State, got.ErrorClass)
	}

	wantKey := sessionkey.CronKey(j.ID)
	resets, _ := router.snapshot()
	resetCount := 0
	for _, k := range resets {
		if k == wantKey {
			resetCount++
		}
	}
	// Preflight Reset (run start) + post-error reap Reset = at least 2.
	if resetCount < 2 {
		t.Errorf("Reset(%q) count = %d, want >=2 (preflight + error-path reap); "+
			"resets=%v — session-error path must Reset the fresh session before releasing the CAS gate (R20260608133928-GO-7)",
			wantKey, resetCount, resets)
	}
	// The ordering itself, which used to be a regexp over scheduler_run.go:
	// finishRun emits run-ended AND releases the CAS gate, so a Reset landing
	// after it ran outside the gate.
	// >=2: the preflight Reset AND the error-path reap must both land inside the
	// gate. Asserting only "some reset precedes run-ended" is satisfied by the
	// preflight, which lets a deferred reap slip through.
	ord.assertCountBefore(t, "reset", 2, "run-ended",
		"session-error path must Reset the fresh session while the CAS gate is held (R20260608133928-GO-7)")
}

// TestFreshGetSession_CancelError_ResetsBeforeFinishRun verifies that when
// fresh-mode GetOrCreate returns context.Canceled (graceful shutdown race),
// Reset(cronKey) is called before finishRun records the cancellation.
func TestFreshGetSession_CancelError_ResetsBeforeFinishRun(t *testing.T) {
	t.Parallel()

	ord := &orderRecorder{}
	rec := &recordingBroadcaster{order: ord}
	router := &cancelGetSessionRouter{reapRouter: reapRouter{order: ord}}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-fresh-getsess-cancel", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow: skip jitter */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateCanceled {
		t.Fatalf("state: want canceled, got %q (errClass=%q)", got.State, got.ErrorClass)
	}

	wantKey := sessionkey.CronKey(j.ID)
	resets, _ := router.snapshot()
	resetCount := 0
	for _, k := range resets {
		if k == wantKey {
			resetCount++
		}
	}
	// Preflight Reset (run start) + post-cancel reap Reset = at least 2.
	if resetCount < 2 {
		t.Errorf("Reset(%q) count = %d, want >=2 (preflight + cancel-path reap); "+
			"resets=%v — GetOrCreate cancel path must Reset the fresh session before releasing the CAS gate (R20260608133928-GO-7)",
			wantKey, resetCount, resets)
	}
	// The ordering itself, which used to be a regexp over scheduler_run.go:
	// finishRun emits run-ended AND releases the CAS gate, so a Reset landing
	// after it ran outside the gate.
	// >=2: the preflight Reset AND the error-path reap must both land inside the
	// gate. Asserting only "some reset precedes run-ended" is satisfied by the
	// preflight, which lets a deferred reap slip through.
	ord.assertCountBefore(t, "reset", 2, "run-ended",
		"GetOrCreate cancel path must Reset the fresh session while the CAS gate is held (R20260608133928-GO-7)")
}

// TestPersistentGetSession_SessionError_NoReset verifies that persistent-mode
// (FreshContext=false) jobs do NOT have Reset called on GetOrCreate error —
// Reset would destroy the reused persistent session unnecessarily.
func TestPersistentGetSession_SessionError_NoReset(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	router := &errGetSessionRouter{
		reapRouter: reapRouter{},
		getErr:     errors.New("session backend unavailable"),
	}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-persist-getsess-err", Schedule: "@every 5m", Prompt: "ping", FreshContext: false}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true)

	wantKey := sessionkey.CronKey(j.ID)
	resets, _ := router.snapshot()
	for _, k := range resets {
		if k == wantKey {
			t.Errorf("persistent-mode job must not be Reset on GetOrCreate error; got Reset(%q) in %v",
				wantKey, resets)
		}
	}
}
