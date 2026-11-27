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
	s.putJobForTest(j)
	if err := s.registerJobForTest(j); err != nil {
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

// TestRegisterJob_ManualOnlySkipsTicks: an isolated instance's scheduler
// (ManualOnly) skips the tick but still runs a job started by hand.
func TestRegisterJob_ManualOnlySkipsTicks(t *testing.T) {
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, ManualOnly: true}, SchedulerDeps{Router: &reapRouter{sid: "sess-tick"}, Telemetry: rec})

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 1h", Prompt: "ping"}
	s.putJobForTest(j)
	if err := s.registerJobForTest(j); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	s.cron.Entry(j.entryID).Job.Run()
	if n := rec.endedCount(); n != 0 {
		t.Fatalf("a ManualOnly scheduler ran a scheduled tick (%d ended events)", n)
	}
	s.executeIfNotDeletedOrPaused(jobID)
	if n := rec.endedCount(); n != 1 {
		t.Fatalf("a manual run on a ManualOnly scheduler produced %d ended events, want 1", n)
	}
}

// TestRegisterJob_TickSkipsPausedOrDeletedJob: the tick callback goes through
// the shared paused/deleted gate and looks the job up by ID at fire time, so a
// pause or delete after registration stops the run.
func TestRegisterJob_TickSkipsPausedOrDeletedJob(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, s *Scheduler, id string)
	}{
		{"paused", func(t *testing.T, s *Scheduler, id string) {
			s.editJobForTest(t, id, func(j *Job) { j.Paused = true })
		}},
		{"deleted", func(_ *testing.T, s *Scheduler, id string) { s.dropJobForTest(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingBroadcaster{}
			s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &reapRouter{sid: "sess-tick"}, Telemetry: rec})
			jobID := mustGenerateID()
			j := &Job{ID: jobID, Schedule: "@every 1h", Prompt: "ping"}
			s.putJobForTest(j)
			if err := s.registerJobForTest(j); err != nil {
				t.Fatalf("registerJob: %v", err)
			}
			entry := s.cron.Entry(j.entryID)
			tc.after(t, s, jobID)
			entry.Job.Run()
			if n := rec.endedCount(); n != 0 {
				t.Errorf("the tick ran a %s job (%d ended events)", tc.name, n)
			}
		})
	}
}
