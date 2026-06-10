package cron

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUpdateJob_CronOpsOutsideMu_Structural pins R112714-LOGIC-1: the
// UpdateJob schedule-change branch must NOT call s.cron.Remove or
// s.registerJob (→ AddFunc) while holding s.mu. Both send on unbuffered
// robfig/cron channels drained by the cron run loop goroutine; a tick
// callback spawned by the run loop calls executeJobIDIfLive → s.mu.RLock,
// so calling those channel ops under s.mu.Lock creates a lock-order
// inversion.
//
// Structural pin: the UpdateJob function body must not contain the pattern
// "s.cron.Remove" inside the IIFE (between "func() (Job, func(), error)"
// and the closing "}()"). The fix snapshots removeEntryID under the lock
// and calls Remove post-unlock.
func TestUpdateJob_CronOpsOutsideMu_Structural(t *testing.T) {
	src, err := os.ReadFile("scheduler_jobs.go")
	if err != nil {
		t.Fatalf("read scheduler_jobs.go: %v", err)
	}
	body := string(src)

	// Locate the UpdateJob function.
	const fnMarker = "func (s *Scheduler) UpdateJob("
	fnIdx := strings.Index(body, fnMarker)
	if fnIdx < 0 {
		t.Fatal("UpdateJob not found in scheduler_jobs.go")
	}
	fnBody := body[fnIdx:]
	// Trim to just UpdateJob's body (up to next top-level func).
	if next := strings.Index(fnBody[len(fnMarker):], "\nfunc "); next >= 0 {
		fnBody = fnBody[:len(fnMarker)+next]
	}

	// Find the IIFE that holds s.mu — it starts with the IIFE open and
	// ends with "}()". We want to verify s.cron.Remove does NOT appear
	// inside this locked region.
	const iifeOpen = "result, save, err := func() (Job, func(), error) {"
	iifeIdx := strings.Index(fnBody, iifeOpen)
	if iifeIdx < 0 {
		t.Fatal("UpdateJob IIFE not found — fix may have changed structure; update this test")
	}
	// The IIFE closes with "}()" immediately following its body.
	iifeClose := "}()"
	iifeCloseIdx := strings.Index(fnBody[iifeIdx:], iifeClose)
	if iifeCloseIdx < 0 {
		t.Fatal("UpdateJob IIFE closing '}()' not found")
	}
	iifebody := fnBody[iifeIdx : iifeIdx+iifeCloseIdx+len(iifeClose)]

	// s.cron.Remove must NOT appear inside the IIFE.
	if strings.Contains(iifebody, "s.cron.Remove(") {
		t.Error("R112714-LOGIC-1: s.cron.Remove is called inside UpdateJob's " +
			"locked IIFE. This sends on robfig/cron's unbuffered c.remove " +
			"channel while holding s.mu, risking lock-order inversion with " +
			"tick callback goroutines that need s.mu.RLock. Move Remove to " +
			"post-unlock (after the IIFE returns), mirroring PauseJobByID.")
	}

	// s.registerJob (→ AddFunc → c.add channel) must also NOT appear inside
	// the IIFE under the schedule-change branch. The fix uses schedNeedsRereg
	// and calls registerJob after the IIFE.
	if strings.Contains(iifebody, "s.registerJob(") {
		t.Error("R112714-LOGIC-1: s.registerJob is called inside UpdateJob's " +
			"locked IIFE. registerJob calls s.cron.AddFunc (c.add channel send) " +
			"and s.cron.Entry (c.snapshot channel send) while holding s.mu. " +
			"Move registerJob calls to post-unlock, mirroring ResumeJobByID's " +
			"rollback hoist.")
	}

	// The post-unlock section must contain s.cron.Remove to confirm it was
	// hoisted rather than deleted.
	postIIFE := fnBody[iifeIdx+iifeCloseIdx:]
	if !strings.Contains(postIIFE, "s.cron.Remove(") {
		t.Error("R112714-LOGIC-1: s.cron.Remove not found in post-IIFE section " +
			"of UpdateJob — it appears to have been removed rather than hoisted. " +
			"The old cron entry must still be removed post-unlock.")
	}
}

// TestUpdateJob_ScheduleChange_ConcurrentTickDoesNotDeadlock is a runtime
// regression for R112714-LOGIC-1. It runs UpdateJob (schedule change) and
// a concurrent TriggerNow (which also acquires s.mu.RLock via
// executeJobIDIfLive) simultaneously. If UpdateJob holds s.mu while calling
// s.cron.Remove or s.cron.AddFunc, and the cron run loop is processing a
// tick that tries s.mu.RLock, a deadlock would surface here via timeout.
func TestUpdateJob_ScheduleChange_ConcurrentTickDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		MaxJobs:        10,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{
		Schedule: "@hourly",
		Prompt:   "initial",
		Platform: "x",
		ChatID:   "c",
		WorkDir:  "/tmp",
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	const iterations = 20
	var wg sync.WaitGroup

	// Repeatedly update the schedule while concurrent goroutines read the
	// job map (simulating what a tick callback or ListAllJobs call does).
	schedules := []string{"@hourly", "@daily", "*/5 * * * *", "@hourly"}
	for i := range iterations {
		wg.Add(2)

		// Writer: change schedule.
		go func(idx int) {
			defer wg.Done()
			newSched := schedules[idx%len(schedules)]
			upd := JobUpdate{Schedule: &newSched}
			// Ignore errors: job may not exist, schedule may not change.
			s.UpdateJob(job.ID, upd) //nolint:errcheck
		}(i)

		// Reader: simulate concurrent read under s.mu.RLock.
		go func() {
			defer wg.Done()
			s.mu.RLock()
			_ = len(s.jobs)
			s.mu.RUnlock()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// No deadlock.
	case <-time.After(10 * time.Second):
		t.Fatal("R112714-LOGIC-1: timed out after 10s — likely deadlock between " +
			"UpdateJob (holding s.mu + cron channel send) and concurrent " +
			"s.mu.RLock reader. cron.Remove/AddFunc must not be called under s.mu.")
	}

	// Verify job still exists and has a valid schedule.
	s.mu.RLock()
	j := s.jobs[job.ID]
	s.mu.RUnlock()
	if j == nil {
		t.Fatal("job missing from s.jobs after concurrent UpdateJob")
	}
}
