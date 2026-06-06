package cron

import (
	"context"
	"sync"
	"testing"
	"time"
)

// stepClock advances by a fixed delta on every Now() call. A full executeOpt
// run reads the clock twice for the lifecycle anchors that bound DurationMS:
// startedAt (executeOpt) and endedAt (finishRun). With a 250ms step the run's
// reported DurationMS is therefore a deterministic multiple of the step,
// independent of real wall-clock — exactly the sleep-free determinism #643
// introduces the clock to enable.
type stepClock struct {
	mu   sync.Mutex
	cur  time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(c.step)
	return c.cur
}

// okSession is a stub Session whose Send always succeeds instantly.
type okSession struct{ id string }

func (s okSession) Send(ctx context.Context, text string) (SendResult, error) {
	return SendResult{Text: "done", SessionID: s.id}, nil
}
func (s okSession) SessionID() string                     { return s.id }
func (s okSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }

// okRouter hands back a ready okSession so executeOpt reaches the success path.
type okRouter struct{ sid string }

func (r okRouter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chain []string) {}
func (r okRouter) Reset(key string)                                                            {}
func (r okRouter) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error) {
	return okSession{id: r.sid}, SessionExisting, nil
}

// TestR247ARCH11_RunDurationDeterministicUnderClock pins #643: a full
// executeOpt run computes DurationMS purely from the injected clock, so a
// step clock yields a stable, sleep-free duration. A regression reverting
// either startedAt or endedAt to a raw time.Now() would make DurationMS
// real-wall-clock-dependent (≈0ms for this instant run) instead of the fixed
// 250ms the step clock dictates (endedAt = startedAt + one 250ms step).
func TestR247ARCH11_RunDurationDeterministicUnderClock(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	clk := &stepClock{cur: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC), step: 250 * time.Millisecond}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:   5,
		Router:    okRouter{sid: "sess-1"},
		Telemetry: rec,
	})
	s.clock = clk

	j := &Job{ID: "job-clock-run", Schedule: "@every 5m", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow: skip jitter for determinism */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateSucceeded {
		t.Fatalf("state: want succeeded, got %q (err=%q)", got.State, got.ErrorClass)
	}
	// startedAt = first Now() (+250ms), endedAt = second Now() (+250ms more);
	// DurationMS = 250.
	if got.DurationMS != 250 {
		t.Errorf("DurationMS = %d, want 250 (run duration must derive from the injected clock)", got.DurationMS)
	}
}
