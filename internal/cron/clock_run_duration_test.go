package cron

import (
	"context"
	"testing"
	"time"
)

// okSession is a stub Session whose Send always succeeds instantly.
type okSession struct{ id string }

func (s okSession) Send(ctx context.Context, text string) (SendResult, error) {
	return SendResult{Text: "done", SessionID: s.id}, nil
}
func (s okSession) SessionID() string                     { return s.id }
func (s okSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }

// TestRunDurationDeterministicUnderClock pins #643: a full
// executeOpt run computes DurationMS purely from the injected clock, so a
// step clock yields a stable, sleep-free duration. A regression reverting
// either startedAt or endedAt to a raw time.Now() would make DurationMS
// real-wall-clock-dependent (≈0ms for this instant run) instead of the fixed
// 250ms the step clock dictates (endedAt = startedAt + one 250ms step).
func TestRunDurationDeterministicUnderClock(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	clk := &fakeClock{now: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC), step: 250 * time.Millisecond}
	s := NewScheduler(SchedulerConfig{
		MaxJobs: 5,
	}, SchedulerDeps{
		Router:    &reapRouter{sid: "sess-1"},
		Telemetry: rec,
	})
	s.clock = clk

	j := &Job{ID: "job-clock-run", Schedule: "@every 5m", Prompt: "ping"}
	s.putJobForTest(j)

	s.executeOpt(j.ID, true /* viaTriggerNow: skip jitter for determinism */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateSucceeded {
		t.Fatalf("state: want succeeded, got %q (err=%q)", got.State, got.ErrorClass)
	}
	// Clock reads during executeOpt with a step-clock (step=250ms):
	//   1. startedAt (executeOpt entry) = T0 + 250ms
	//   2. spawnStart = s.now() (R20260607-GO-002: executeGetSession) = T0 + 500ms
	//   3. endedAt (finishRun) = T0 + 750ms
	// DurationMS = endedAt - startedAt = 500ms.
	if got.DurationMS != 500 {
		t.Errorf("DurationMS = %d, want 500 (run duration must derive from the injected clock)", got.DurationMS)
	}
}
