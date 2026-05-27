package cron

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestExecuteOpt_PostCASPausedRecheck_TriggerNow exercises the
// R20260527122801-CR-8 (#1322) fix: a Pause that lands AFTER the dispatch
// helper releases s.mu but BEFORE executeOpt's CAS gate must abort the
// run. Without the post-CAS recheck the TriggerNow path would reach the
// router.GetOrCreate call and burn a real run on a paused job.
//
// Strategy: register a job, then mark it Paused under s.mu (simulating
// the post-RLock-release / pre-CAS race window) before calling executeOpt
// directly with viaTriggerNow=true. The router stub increments a counter
// on GetOrCreate; the test asserts the counter stays at zero.
func TestExecuteOpt_PostCASPausedRecheck_TriggerNow(t *testing.T) {
	t.Parallel()

	sr := &jitterStubRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:    sr,
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   10,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{
		ID:       "paused-via-race",
		Schedule: "@every 1h",
		Prompt:   "should-not-run",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Simulate the cross-lock race: by the time executeOpt runs, the job
	// has been paused. Mutate Paused directly under s.mu so we don't
	// require a full PauseJobByID dance (which would also tear down the
	// cron entry and is orthogonal to this test).
	s.mu.Lock()
	s.jobs[j.ID].Paused = true
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.executeOpt(j, true /* viaTriggerNow */)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("executeOpt blocked >2s; post-CAS paused recheck did not abort early")
	}

	if got := atomic.LoadInt64(&sr.calls); got != 0 {
		t.Fatalf("router.GetOrCreate called %d times; expected 0 — paused recheck failed", got)
	}
}

// TestExecuteOpt_PostCASDeletedRecheck_TriggerNow mirrors the paused test
// for the deleted-job side of the race window — a DeleteJob that lands
// between dispatch and CAS must also abort the TriggerNow run before any
// router work.
func TestExecuteOpt_PostCASDeletedRecheck_TriggerNow(t *testing.T) {
	t.Parallel()

	sr := &jitterStubRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:    sr,
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   10,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{
		ID:       "deleted-via-race",
		Schedule: "@every 1h",
		Prompt:   "should-not-run",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Simulate the dispatch-vs-delete race: caller resolved cur from
	// s.jobs, released RLock, and a Delete landed before CAS. We can't
	// call DeleteJobByID because it also runs router.Reset / postCleanup
	// in ways that aren't on the hot path of this test; mutating the map
	// directly is the smallest reproduction of the executeOpt-visible
	// state.
	s.mu.Lock()
	delete(s.jobs, j.ID)
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.executeOpt(j, true /* viaTriggerNow */)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("executeOpt blocked >2s; post-CAS deleted recheck did not abort early")
	}

	if got := atomic.LoadInt64(&sr.calls); got != 0 {
		t.Fatalf("router.GetOrCreate called %d times; expected 0 — deleted recheck failed", got)
	}
}
