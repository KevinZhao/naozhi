package cron

// CRON3 regression tests. Without the stopCtx.Err() guard in execute(),
// a fresh-context cron tick that fired just before Scheduler.Stop() took
// the cancel edge could still reach Router.Reset + Router.GetOrCreate
// after Router.Shutdown had cleared sessions[], leaking a brand-new CLI
// process tied to an orphan "cron:<id>" key that outlives naozhi (shim
// cleanup only covers sessions that were in the map at Shutdown).
//
// These tests drive execute() directly via the existing fakeSessionRouter
// from session_router_test.go so they can assert which router APIs the
// scheduler reaches for without spinning up a real *session.Router.

import (
	"testing"
)

// TestCRON3_FreshExecuteSkippedAfterStopCtxCancel locks in that execute()
// returns early (no Reset + no GetOrCreate) when stopCtx is already
// cancelled before execute() enters the fresh branch. This is the narrow
// shutdown-overlap window that used to leak orphan CLI processes.
func TestCRON3_FreshExecuteSkippedAfterStopCtxCancel(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs: 5,
	}, SchedulerDeps{
		Router: fake,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Seed a fresh-mode job.
	freshTrue := true
	job := &Job{
		Schedule:     "@hourly",
		Prompt:       "test",
		Platform:     "p",
		ChatID:       "c",
		FreshContext: true,
		Notify:       &freshTrue, // arbitrary, unused by the assertion
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Baseline: one RegisterCronStub from AddJob.
	fake.mu.Lock()
	baselineRegister := len(fake.registerCalls)
	baselineReset := len(fake.resetCalls)
	baselineGetCreate := len(fake.getCreateKeys)
	fake.mu.Unlock()

	// Now cancel stopCtx by stopping the scheduler, then drive execute()
	// synchronously. The guard must keep Reset + GetOrCreate at zero for
	// this run.
	s.Stop()

	// s.execute is unexported; we re-enter through the stored Job pointer
	// that AddJob pinned into s.tbl.jobs. execute() is safe to call on a
	// stopped scheduler — the guard is exactly what we're testing.
	s.executeOpt(job.ID, false)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := len(fake.resetCalls) - baselineReset; got != 0 {
		t.Errorf("Reset calls after shutdown = %d, want 0 (guard failed)", got)
	}
	if got := len(fake.getCreateKeys) - baselineGetCreate; got != 0 {
		t.Errorf("GetOrCreate calls after shutdown = %d, want 0 (guard failed)", got)
	}
	// RegisterCronStub should not have grown either (the skipped path
	// never reaches stubRefresh).
	if got := len(fake.registerCalls) - baselineRegister; got != 0 {
		t.Errorf("RegisterCronStub calls after shutdown = %d, want 0", got)
	}
}

// TestCRON3_PersistentModeUnaffectedByGuard: the stopCtx.Err() check lives
// *inside* the fresh branch so persistent-mode jobs reach GetOrCreate and
// let the router handle ctx cancellation (already Round 20-21 logic).
// This test pins the divergence in behaviour so a future refactor that
// lifts the guard out of the fresh branch — and therefore suppresses
// persistent executions too — fails this regression.
func TestCRON3_PersistentModeUnaffectedByGuard(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs: 5,
	}, SchedulerDeps{
		Router: fake,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job := &Job{
		Schedule:     "@hourly",
		Prompt:       "test",
		Platform:     "p",
		ChatID:       "c",
		FreshContext: false, // persistent mode
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	fake.mu.Lock()
	baselineGetCreate := len(fake.getCreateKeys)
	fake.mu.Unlock()

	// Persistent-mode execute *after* Stop: the guard under test is only
	// in the fresh branch, so GetOrCreate must still be invoked (the fake
	// returns nil session + nil err, so execute() records a benign result
	// and returns). Router.Shutdown in the real system is what cancels
	// the downstream ctx; the cron path intentionally delegates that.
	s.Stop()
	func() {
		defer func() { _ = recover() }() // absorb nil-session Send panic
		s.executeOpt(job.ID, false)
	}()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := len(fake.getCreateKeys) - baselineGetCreate; got != 1 {
		t.Errorf("persistent mode GetOrCreate after Stop = %d, want 1 (guard over-fired)", got)
	}
	if len(fake.resetCalls) != 0 {
		t.Errorf("persistent mode should never call Reset, got %v", fake.resetCalls)
	}
}
