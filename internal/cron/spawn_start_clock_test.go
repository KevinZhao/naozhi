package cron

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// TestR20260607GO002_SpawnStartUsesInjectedClock pins R20260607-GO-002:
// executeGetSession must capture spawnStart via s.now() (not time.Now()) so
// tests can inject a fake clock and observe a deterministic spawnStart without
// real sleeps.
//
// Strategy: pre-cancel the spawn context so GetOrCreate returns
// context.Canceled immediately. executeGetSession returns abort=true with the
// spawnStart it computed. We assert that value equals the fake clock's fixed
// instant.
func TestR20260607GO002_SpawnStartUsesInjectedClock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fixed := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: fixed}

	sched := NewScheduler(SchedulerConfig{
		MaxJobs: 5,
		// GetOrCreate returns nil, SessionExisting, nil
		StorePath: dir + "/cron_jobs.json",
	}, SchedulerDeps{
		Router: &fakeRouter{},
	})
	sched.clock = clk

	j := &Job{ID: "job-spawn-clock", Schedule: "@every 5m", Prompt: "ping"}
	sched.mu.Lock()
	sched.jobs[j.ID] = j
	sched.mu.Unlock()

	inflight := sched.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	defer inflight.running.Store(false)
	finalizer := &runFinalizer{inflight: inflight}

	// Use a fakeRouter that returns context.Canceled so executeGetSession
	// returns abort=true immediately. The spawned context is already
	// cancelled before the call, and the router is wired to propagate it.
	cancelRouter := &fakeRouter{getErr: context.Canceled}
	sched.router = cancelRouter

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, spawnStart, abort := sched.executeGetSession(getSessionArgs{
		ctx:         ctx,
		spawnCancel: cancel,
		key:         "feishu:private:u-spawn-clock",
		opts:        AgentOpts{},
		job:         j,
		snap:        jobSnapshot{jobID: j.ID},
		runID:       "r-spawn-clock",
		startedAt:   fixed,
		trigger:     TriggerScheduled,
		lg:          slog.Default(),
		notifyTo:    NotifyTarget{},
		finalizer:   finalizer,
		stubRefresh: stubRefresher{}, // active=false → run() is a no-op
		inflight:    inflight,
	})

	if !abort {
		t.Fatal("expected abort=true from cancelled context")
	}
	if !spawnStart.Equal(fixed) {
		t.Errorf("spawnStart = %v, want injected clock %v (s.now() not used)", spawnStart, fixed)
	}
}
