package cron

import (
	"context"
	"errors"
	"os"
	"regexp"
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

// TestFreshReapErrorPath_SourceAnchor_ResetBeforeFinishRun is the
// R20260608-CORR-1 (#1956) source anchor: the send-error branch of executeOpt
// MUST call `if snap.fresh { s.router.Reset(key) }` BEFORE the finishRun that
// releases the inflight CAS gate. The ordering mirrors the success-path
// R050103A-COUPLING-1 (#1911) contract. A regression that moves Reset back
// after finishRun re-opens the race where a concurrent TriggerNow wins the CAS
// and run-A's late Reset blindly tears down run-B's fresh session.
//
// We pin this with a source-order check rather than a race detector test
// because the window is too narrow to reliably reproduce at runtime — the
// structural guard is the right tool for an ordering invariant.
func TestFreshReapErrorPath_SourceAnchor_ResetBeforeFinishRun(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	body := string(src)

	// Locate the R20260608-CORR-1 anchor comment, which marks both Reset
	// moves (cancel + send-error). We require at least two occurrences to
	// confirm both branches were patched.
	anchorRe := regexp.MustCompile(`R20260608-CORR-1`)
	anchors := anchorRe.FindAllStringIndex(body, -1)
	if len(anchors) < 2 {
		t.Errorf("scheduler_run.go: expected >=2 R20260608-CORR-1 anchors (one per branch: cancel + send-error), got %d; "+
			"both branches must carry the ordering comment to stay auditable (#1956)", len(anchors))
	}

	// For each patched branch the if-snap.fresh-Reset block must appear
	// BEFORE the immediately following finishRun call. We walk the source
	// positions of all `if snap.fresh {` guarded Reset calls and all
	// `s.finishRun(finishArgs{` calls and verify that every guarded-Reset
	// position is followed by at least one finishRun that is closer than the
	// NEXT guarded-Reset.
	resetBlockRe := regexp.MustCompile(`if snap\.fresh \{\s*\n\s*s\.router\.Reset\(key\)`)
	finishRunRe := regexp.MustCompile(`s\.finishRun\(finishArgs\{`)

	resetMatches := resetBlockRe.FindAllStringIndex(body, -1)
	finishMatches := finishRunRe.FindAllStringIndex(body, -1)

	if len(resetMatches) == 0 {
		t.Fatal("scheduler_run.go: no `if snap.fresh { s.router.Reset(key) }` blocks found; " +
			"the error/cancel-path reap guard must be present (#1956)")
	}
	if len(finishMatches) == 0 {
		t.Fatal("scheduler_run.go: no finishRun(finishArgs{) calls found")
	}

	// Each snap.fresh Reset block must be followed by a finishRun before the
	// next snap.fresh Reset block (or end of file).
	for i, rm := range resetMatches {
		// upper bound: next reset block start, or EOF
		upperBound := len(body)
		if i+1 < len(resetMatches) {
			upperBound = resetMatches[i+1][0]
		}
		found := false
		for _, fm := range finishMatches {
			if fm[0] > rm[1] && fm[0] < upperBound {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scheduler_run.go: `if snap.fresh { s.router.Reset(key) }` block at offset %d is NOT "+
				"immediately followed by a finishRun(finishArgs{) call before the next Reset block (or EOF); "+
				"Reset must precede finishRun so the CAS gate is still held during Reset (#1956)",
				rm[0])
		}
	}
}

// TestFreshContextResetsOnSendError pins the behavioral half of #1956:
// a fresh-context cron job where Send returns an error MUST call
// router.Reset(cronKey) — so the exempt CLI session (~1.6 GB) is reclaimed
// and the run-A Reset cannot race run-B's new session after the gate drops.
//
// This mirrors TestFreshContextReapsSessionAfterSuccess for the error path.
func TestFreshContextResetsOnSendError(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	router := &errSendRouter{
		reapRouter: reapRouter{},
		sess:       errSendSession{sendErr: errors.New("send failed: connection reset")},
	}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

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
}

// TestFreshContextResetsOnCancel pins the behavioral half of #1956 for the
// cancel branch: a fresh-context cron job whose Send returns context.Canceled
// MUST also call router.Reset(cronKey) to reclaim the exempt session.
func TestFreshContextResetsOnCancel(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	router := &errSendRouter{
		reapRouter: reapRouter{},
		sess:       cancelSendSession{},
	}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

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
}
