package cron

import (
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// fakeClock is a deterministic cronClock for tests: Now() returns a fixed
// instant so lifecycle timestamps (finishRun endedAt, synthetic-skipped
// startedAt) are pinned without sleeping. R247-ARCH-11 (#643).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
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

// TestR247ARCH11_FinishRunUsesInjectedClock pins #643: finishRun reads
// endedAt from the scheduler's injected clock, so DurationMS is deterministic
// and a fake clock can drive a fixed duration without sleeping. A regression
// that reverted finishRun to a raw time.Now() would compute DurationMS from
// real wall-clock (≈0ms for an instantly-completing test) instead of the
// 1500ms the fake clock dictates.
func TestR247ARCH11_FinishRunUsesInjectedClock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bc := &durationCapturingBroadcaster{}
	clk := &fakeClock{now: time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)}

	cfg := SchedulerConfig{
		MaxJobs:   5,
		StorePath: dir + "/cron_jobs.json",
	}
	sched := NewScheduler(cfg, SchedulerDeps{Router: &fakeRouter{}, Telemetry: bc})
	// Inject the fake clock after construction (the default real clock is
	// installed by NewScheduler). Direct field write is safe: no tick
	// goroutines run in this isolated finishRun test.
	sched.clock = clk

	j := &Job{ID: "job-clock", Schedule: "@every 5m", Prompt: "ping"}
	sched.mu.Lock()
	sched.jobs[j.ID] = j
	sched.mu.Unlock()

	inflight := sched.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	// startedAt is 1500ms before the clock's fixed "now"; finishRun should
	// read endedAt from the clock and compute DurationMS = 1500.
	startedAt := clk.Now().Add(-1500 * time.Millisecond)
	// Advance the clock between started and finish to prove finishRun reads
	// the live clock value at finish time, not a captured constant.
	clk.set(startedAt.Add(1500 * time.Millisecond))

	sched.finishRun(finishArgs{
		job:       j,
		runID:     "r-clock",
		startedAt: startedAt,
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		result:    "ok",
		finalizer: finalizer,
	})

	bc.mu.Lock()
	defer bc.mu.Unlock()
	if !bc.sawEnded {
		t.Fatal("expected a cron_run_ended broadcast")
	}
	if bc.durationMS != 1500 {
		t.Errorf("DurationMS = %d, want 1500 (finishRun must read endedAt from injected clock)", bc.durationMS)
	}
	if !bc.endedAt.Equal(clk.Now()) {
		t.Errorf("EndedAt = %v, want injected clock now %v", bc.endedAt, clk.Now())
	}
}

// TestR247ARCH11_SyntheticSkippedUsesInjectedClock pins that the
// synthetic started→ended pair (emitSyntheticSkipped, used by overlap-skipped
// and router-missing guards) also stamps its startedAt from the injected
// clock. Skipped runs report DurationMS=0 (started==ended under a fixed
// clock), and EndedAt equals the clock's now — proving both timestamps flow
// through s.now() rather than the real wall clock.
func TestR247ARCH11_SyntheticSkippedUsesInjectedClock(t *testing.T) {
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

	sched.emitOverlapSkipped(j, false)

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
