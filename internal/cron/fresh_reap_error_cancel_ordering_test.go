package cron

import (
	"context"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// errSendSession is a stub Session whose Send always returns a fixed error.
// Used to drive the send-error branch of executeOpt without a real CLI.
type errSendSession struct {
	sendErr error
}

func (s errSendSession) Send(_ context.Context, _ string) (SendResult, error) {
	return SendResult{}, s.sendErr
}
func (s errSendSession) SessionID() string                     { return "" }
func (s errSendSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }

// cancelSendSession is a stub Session whose Send returns context.Canceled.
// Drives the cancel branch of executeOpt's error handling.
type cancelSendSession struct{}

func (s cancelSendSession) Send(_ context.Context, _ string) (SendResult, error) {
	return SendResult{}, context.Canceled
}
func (s cancelSendSession) SessionID() string                     { return "" }
func (s cancelSendSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }

// errSendRouter hands back a Session whose Send returns a fixed error.
type errSendRouter struct {
	reapRouter
	sess Session
}

func (r *errSendRouter) GetOrCreate(_ context.Context, _ string, _ AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionExisting, nil
}

// TestFreshContextResetsOnSendError pins the behavioral half of #1956:
// a fresh-context cron job where Send returns an error MUST call
// router.Reset(cronKey) — so the exempt CLI session (~1.6 GB) is reclaimed
// and the run-A Reset cannot race run-B's new session after the gate drops.
//
// This mirrors TestFreshContextReapsSessionAfterSuccess for the error path.
func TestFreshContextResetsOnSendError(t *testing.T) {
	t.Parallel()

	ord := &orderRecorder{}
	rec := &recordingBroadcaster{order: ord}
	router := &errSendRouter{
		reapRouter: reapRouter{order: ord},
		sess:       errSendSession{sendErr: errors.New("send failed: connection reset")},
	}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-fresh-send-err", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow: skip jitter */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateFailed {
		t.Fatalf("state: want failed, got %q (err=%q)", got.State, got.ErrorClass)
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
		t.Errorf("Reset(%q) count = %d, want >=2 (preflight + post-error reap); "+
			"resets=%v — error path must Reset the exempt session before releasing the CAS gate (#1956)",
			wantKey, resetCount, resets)
	}
	// The ordering itself, which used to be a regexp over scheduler_run.go.
	// >=2: preflight AND reap must both land inside the gate — asserting only
	// "some reset precedes run-ended" is satisfied by the preflight alone, which
	// lets a deferred reap slip through.
	ord.assertCountBefore(t, "reset", 2, "run-ended",
		"error path must Reset the exempt session while the CAS gate is held (#1956)")
	// The stub re-register on this branch must also precede the gate release:
	// execSendError non-cancel branch (R202606h-GO-009/GO-009b/GO-010). This replaces the
	// ">=5 stub-refresh call sites" count in the deleted source anchor.
	ord.assertCountBefore(t, "register-stub", 1, "run-ended",
		"execSendError non-cancel branch must re-register the stub while the CAS gate is held (R202606h-GO-009)")
}

// TestFreshContextResetsOnCancel pins the behavioral half of #1956 for the
// cancel branch: a fresh-context cron job whose Send returns context.Canceled
// MUST also call router.Reset(cronKey) to reclaim the exempt session.
func TestFreshContextResetsOnCancel(t *testing.T) {
	t.Parallel()

	ord := &orderRecorder{}
	rec := &recordingBroadcaster{order: ord}
	router := &errSendRouter{
		reapRouter: reapRouter{order: ord},
		sess:       cancelSendSession{},
	}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-fresh-cancel", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow: skip jitter */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateCanceled {
		t.Fatalf("state: want canceled, got %q (err=%q)", got.State, got.ErrorClass)
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
		t.Errorf("Reset(%q) count = %d, want >=2 (preflight + post-cancel reap); "+
			"resets=%v — cancel path must Reset the exempt session before releasing the CAS gate (#1956)",
			wantKey, resetCount, resets)
	}
	// The ordering itself, which used to be a regexp over scheduler_run.go.
	// >=2: preflight AND reap must both land inside the gate — asserting only
	// "some reset precedes run-ended" is satisfied by the preflight alone, which
	// lets a deferred reap slip through.
	ord.assertCountBefore(t, "reset", 2, "run-ended",
		"cancel path must Reset the exempt session while the CAS gate is held (#1956)")
	// The stub re-register on this branch must also precede the gate release:
	// execSendError cancel branch (R202606h-GO-009/GO-009b/GO-010). This replaces the
	// ">=5 stub-refresh call sites" count in the deleted source anchor.
	ord.assertCountBefore(t, "register-stub", 1, "run-ended",
		"execSendError cancel branch must re-register the stub while the CAS gate is held (R202606h-GO-009)")
}
