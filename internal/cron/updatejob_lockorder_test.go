package cron

import (
	"sync"
	"testing"
	"time"
)

// TestUpdateJob_ScheduleChange_SurvivesConcurrentReaders drives UpdateJob's
// schedule-change branch — the one that calls s.cron.Remove and then
// registerJob (the robfig Schedule rendezvous) — against concurrent s.tbl.mu readers, and checks
// that the job survives with a live record.
//
// This file used to also carry TestUpdateJob_CronOpsOutsideMu_Structural, a
// source anchor for R112714-LOGIC-1: "UpdateJob must not call s.cron.Remove or
// s.registerJob while holding s.tbl.mu, because both send on unbuffered
// robfig/cron channels drained by the run loop, and a tick callback reaches
// executeJobIDIfLive → s.tbl.mu.RLock, so holding s.tbl.mu across them inverts the
// lock order." Retired, because neither half of that holds (#2547):
//
// The hazard is not reachable in robfig/cron v3.0.1. Entries, Remove and
// Schedule do each rendezvous with the run loop while holding c.runningMu, so
// the order s.tbl.mu → c.runningMu is real. An inversion needs the opposite order
// somewhere, and there is none: run() never acquires c.runningMu, startJob
// hands the callback to a fresh goroutine holding no locks, and Stop's
// jobWaiter.Wait() runs in a goroutine outside the c.runningMu it took. So
// nothing ever reaches s.tbl.mu with c.runningMu held, and the cycle cannot close.
// A future robfig upgrade could introduce one, but no test in this package can
// see that — it is a dependency-review concern.
//
// And the anchor did not enforce its own rule. It searched only the text
// between UpdateJob's IIFE opener and the first "}()", so:
//
//   - line 552 already calls s.registerJob under s.tbl.mu.Lock (the entryID
//     write-back block), which is exactly what the anchor's message forbids,
//     and the anchor was green;
//   - moving s.cron.Remove into that same s.tbl.mu.Lock — the most direct possible
//     form of the mutation it was written to forbid — also left it green.
//
// Both probed on the real tree before deleting it.
//
// What remains worth checking is this concurrency exercise, which is
// version-independent: it puts UpdateJob's cron ops in flight against s.tbl.mu
// readers, so if the inversion ever does become reachable it wedges here.
func TestUpdateJob_ScheduleChange_SurvivesConcurrentReaders(t *testing.T) {
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

	// Cycling through distinct schedules keeps schedNeedsRereg true, so every
	// iteration really does take the Remove + registerJob path rather than
	// short-circuiting on an unchanged schedule.
	schedules := []string{"@daily", "*/5 * * * *", "@weekly", "@hourly"}
	for i := range iterations {
		wg.Add(2)

		go func(idx int) {
			defer wg.Done()
			newSched := schedules[idx%len(schedules)]
			upd := JobUpdate{Schedule: &newSched}
			s.UpdateJob(job.ID, upd) //nolint:errcheck // racing updates may legitimately fail
		}(i)

		go func() {
			defer wg.Done()
			s.tblForTest().mu.RLock()
			_ = len(s.tblForTest().jobs)
			s.tblForTest().mu.RUnlock()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out after 10s: UpdateJob's schedule-change branch wedged " +
			"against concurrent s.tbl.mu readers")
	}

	s.tblForTest().mu.RLock()
	j := s.tblForTest().jobs[job.ID]
	s.tblForTest().mu.RUnlock()
	if j == nil {
		t.Fatal("job missing from s.tbl.jobs after concurrent UpdateJob")
	}
	if j.entryID == 0 {
		t.Error("job has no cron entry after the schedule-change branch ran; " +
			"Remove landed but registerJob did not")
	}
}
