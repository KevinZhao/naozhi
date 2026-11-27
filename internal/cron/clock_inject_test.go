package cron

import (
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// fakeClock is a deterministic cronClock for tests: Now() returns a fixed
// instant so lifecycle timestamps (finishRun endedAt, synthetic-skipped
// startedAt) are pinned without sleeping. R247-ARCH-11 (#643). A non-zero
// step advances the clock by step before every Now(), so a full executeOpt
// run's DurationMS (startedAt to endedAt) is a deterministic multiple of it.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// durationCapturingBroadcaster records the DurationMS of the cron_run_ended
// event so a test can assert finishRun computed it from the injected clock's
// endedAt rather than the real wall clock.
type durationCapturingBroadcaster struct {
	mu         sync.Mutex
	durationMS int64
	endedAt    time.Time
	sawEnded   bool
}

func (b *durationCapturingBroadcaster) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {}
func (b *durationCapturingBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.durationMS = ev.DurationMS
	b.endedAt = ev.EndedAt
	b.sawEnded = true
}

// TestSyntheticSkippedUsesInjectedClock pins that the
// synthetic started→ended pair (emitSyntheticSkipped, used by overlap-skipped
// and router-missing guards) also stamps its startedAt from the injected
// clock. Skipped runs report DurationMS=0 (started==ended under a fixed
// clock), and EndedAt equals the clock's now — proving both timestamps flow
// through s.now() rather than the real wall clock.
func TestSyntheticSkippedUsesInjectedClock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bc := &durationCapturingBroadcaster{}
	fixed := time.Date(2026, 6, 3, 9, 30, 0, 0, time.UTC)
	clk := &fakeClock{now: fixed}

	cfg := SchedulerConfig{
		MaxJobs:   5,
		StorePath: dir + "/cron_jobs.json",
	}
	sched := NewScheduler(cfg, SchedulerDeps{Router: &fakeRouter{}, Telemetry: bc})
	sched.clock = clk

	j := &Job{ID: "job-skip-clock", Schedule: "@every 5m", Prompt: "ping"}

	sched.emitOverlapSkipped(j.ID, false)

	bc.mu.Lock()
	defer bc.mu.Unlock()
	if !bc.sawEnded {
		t.Fatal("expected a synthetic cron_run_ended broadcast")
	}
	if bc.durationMS != 0 {
		t.Errorf("synthetic-skipped DurationMS = %d, want 0 (started==ended under fixed clock)", bc.durationMS)
	}
	if !bc.endedAt.Equal(fixed) {
		t.Errorf("synthetic-skipped EndedAt = %v, want injected clock %v", bc.endedAt, fixed)
	}
}
