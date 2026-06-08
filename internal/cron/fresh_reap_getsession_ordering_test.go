package cron

import (
	"context"
	"errors"
	"os"
	"regexp"
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

// TestFreshGetSession_SourceAnchor_ResetBeforeFinishRun is the
// R20260608133928-GO-7 source anchor: executeGetSession's cancel and
// session-error branches MUST call `if a.snap.fresh { s.router.Reset(a.key) }`
// BEFORE the finishRun that releases the inflight CAS gate.
// Matches the executeOpt ordering contract established by R20260608-CORR-1
// (#1956).
func TestFreshGetSession_SourceAnchor_ResetBeforeFinishRun(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	body := string(src)

	// Require at least 2 R20260608-CORR-1 anchors inside executeGetSession
	// (one per branch: cancel + session-error). The executeOpt anchors already
	// counted by the prior test are on the same marker so we require >= 4 total
	// across the whole file (2 from executeOpt + 2 from executeGetSession).
	anchorRe := regexp.MustCompile(`R20260608-CORR-1`)
	anchors := anchorRe.FindAllStringIndex(body, -1)
	if len(anchors) < 4 {
		t.Errorf("scheduler_run.go: expected >=4 R20260608-CORR-1 anchors "+
			"(cancel+send-error in executeOpt + cancel+session-error in executeGetSession), got %d; "+
			"all four branches must carry the ordering comment (#1956, R20260608133928-GO-7)",
			len(anchors))
	}

	// Every `if a.snap.fresh {` Reset block must be followed by a finishRun
	// before the next such block (or EOF) — ordering invariant for
	// executeGetSession's a.snap / a.key variant.
	resetBlockRe := regexp.MustCompile(`if a\.snap\.fresh \{\s*\n\s*s\.router\.Reset\(a\.key\)`)
	finishRunRe := regexp.MustCompile(`s\.finishRun\(finishArgs\{`)

	resetMatches := resetBlockRe.FindAllStringIndex(body, -1)
	finishMatches := finishRunRe.FindAllStringIndex(body, -1)

	if len(resetMatches) == 0 {
		t.Fatal("scheduler_run.go: no `if a.snap.fresh { s.router.Reset(a.key) }` blocks found; " +
			"executeGetSession error/cancel-path reap guard must be present (R20260608133928-GO-7)")
	}
	if len(finishMatches) == 0 {
		t.Fatal("scheduler_run.go: no finishRun(finishArgs{) calls found")
	}

	for i, rm := range resetMatches {
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
			t.Errorf("scheduler_run.go: `if a.snap.fresh { s.router.Reset(a.key) }` block at offset %d "+
				"is NOT immediately followed by a finishRun(finishArgs{) call before the next Reset block (or EOF); "+
				"Reset must precede finishRun so the CAS gate is still held during Reset (R20260608133928-GO-7)",
				rm[0])
		}
	}
}

// TestFreshGetSession_SessionError_ResetsBeforeFinishRun verifies that when
// fresh-mode GetOrCreate returns a non-cancel error, Reset(cronKey) is called
// (at least once for the preflight, and once for the error-path reap) before
// finishRun records the failure. Uses an errGetSessionRouter that records Reset
// calls so ordering is observable.
func TestFreshGetSession_SessionError_ResetsBeforeFinishRun(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	router := &errGetSessionRouter{
		reapRouter: reapRouter{},
		getErr:     errors.New("session backend unavailable"),
	}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

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
}

// TestFreshGetSession_CancelError_ResetsBeforeFinishRun verifies that when
// fresh-mode GetOrCreate returns context.Canceled (graceful shutdown race),
// Reset(cronKey) is called before finishRun records the cancellation.
func TestFreshGetSession_CancelError_ResetsBeforeFinishRun(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	router := &cancelGetSessionRouter{reapRouter: reapRouter{}}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

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
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

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
