package cron

import (
	"testing"
)

// cron_tick_dispatch_test.go — invokes the callback registerJob actually handed
// to robfig/cron, rather than reading scheduler_jobs.go for the shape of the code
// that builds it. Epic I #2547.
//
// TestRegisterJob_UsesNewCronTickCallbackFactory pinned three things by text:
// that a newCronTickCallback factory exists with a given signature, that
// registerJob calls it at the registration site instead of an inline closure, and that
// the factory passes executeJobIDIfLive(jobID, false, "cron").
//
// The first two are code organisation: an inline closure doing the same three
// things would be equivalent at runtime, so a test asserting "named factory" is a
// style preference, not an invariant. The third IS an invariant — viaTriggerNow
// decides whether the run reports itself as scheduled or manual — and it is
// observable: pull the entry robfig/cron registered and run it.
//
// The existing tests that mention executeJobIDIfLive call it THEMSELVES with
// (jobID, false, "cron"); none of them checks that the registered callback does.
//
// reapRouter rather than fakeRouter: the latter returns (nil, SessionExisting, nil)
// — a nil Session with no error — so it only exercises failure paths and panics in
// executeGetSession'"'"'s success tail. Found by running this test against it.
func TestRegisterJob_TickReportsScheduledTrigger(t *testing.T) {
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &reapRouter{sid: "sess-tick"}, Telemetry: rec})

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 1h", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()
	if err := s.registerJob(j); err != nil {
		t.Fatalf("registerJob: %v", err)
	}

	// Whatever registerJob handed robfig — factory or closure — this is it.
	entry := s.cron.Entry(j.entryID)
	if entry.Job == nil {
		t.Fatalf("no cron entry registered for job %s; registerJob did not reach commitCronEntry", jobID)
	}
	entry.Job.Run()

	if rec.endedCount() != 1 {
		t.Fatalf("the registered callback produced %d ended events, want 1", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if got.Trigger != TriggerScheduled {
		t.Errorf("trigger = %q, want %q — the cron tick callback must pass viaTriggerNow=false, or every scheduled run is labelled manual in history and the dashboard (R246-ARCH-9 / #785)",
			got.Trigger, TriggerScheduled)
	}
}

// TestRegisterJob_TickSkipsAPausedJob covers the other half of what the factory
// delegates for: executeJobIDIfLive is the shared paused/deleted pre-flight gate,
// so a callback that bypassed it would run a job the operator paused.
func TestRegisterJob_TickSkipsAPausedJob(t *testing.T) {
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &reapRouter{sid: "sess-tick"}, Telemetry: rec})

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 1h", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()
	if err := s.registerJob(j); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	// Pause AFTER registration, the way an operator does: the callback captured
	// jobID by value and must re-read the live job to see this.
	s.mu.Lock()
	s.jobs[jobID].Paused = true
	s.mu.Unlock()

	s.cron.Entry(j.entryID).Job.Run()

	if n := rec.endedCount(); n != 0 {
		t.Errorf("the tick ran a paused job (%d ended events); the callback must go through the shared paused/deleted gate", n)
	}
}

// TestRegisterJob_TickSkipsADeletedJob is the same gate's other case, and it also
// covers the jobID-by-value capture: a callback holding a *Job pointer would still
// find its target after the map entry is gone.
func TestRegisterJob_TickSkipsADeletedJob(t *testing.T) {
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &reapRouter{sid: "sess-tick"}, Telemetry: rec})

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 1h", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()
	if err := s.registerJob(j); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	entry := s.cron.Entry(j.entryID)

	s.mu.Lock()
	delete(s.jobs, jobID)
	s.mu.Unlock()

	entry.Job.Run()

	if n := rec.endedCount(); n != 0 {
		t.Errorf("the tick ran a deleted job (%d ended events); the callback must look the job up by ID at fire time, not hold the *Job it was registered with", n)
	}
}
