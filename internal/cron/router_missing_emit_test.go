package cron

import (
	"testing"
)

// TestExecuteOpt_RouterMissingEmitsTerminalEvent pins R20260527122801-CR-13
// (#1323): when executeOpt's hot-path self-defence trips on a nil router,
// it must still emit a started→ended pair so dashboard subscribers see the
// tick. Previously the short-circuit was log-only — the dashboard "running"
// counter and run-list timeline silently lost the tick. errClass is
// router_missing (not overlap_skipped) so operators can distinguish the
// two degraded states.
func TestExecuteOpt_RouterMissingEmitsTerminalEvent(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	// AllowNilRouter so the constructor doesn't slog.Error at boot — we
	// test the executeOpt-time behaviour, not the boot-time loud-log.
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{
		Telemetry: rec,
	})

	j := &Job{ID: "job-router-missing", Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Direct executeOpt call — bypasses the public TriggerNow goroutine
	// fan-out so the synthetic event lands synchronously and the
	// recordingBroadcaster sees it before t.Run returns.
	s.executeOpt(j, true /* viaTriggerNow */)

	if got := rec.endedCount(); got != 1 {
		t.Fatalf("want 1 ended event from router-missing short-circuit, got %d", got)
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateSkipped {
		t.Errorf("state: want skipped, got %q", got.State)
	}
	if got.ErrorClass != ErrClassRouterMissing {
		t.Errorf("error_class: want router_missing, got %q", got.ErrorClass)
	}
	if got.JobID != "job-router-missing" {
		t.Errorf("job_id: want job-router-missing, got %q", got.JobID)
	}
	if got.Trigger != TriggerManual {
		t.Errorf("trigger: want manual, got %q", got.Trigger)
	}
}

// TestExecuteOpt_RouterMissingNilJobNoPanic pins the defensive nil-j guard
// in the short-circuit's emitSyntheticSkipped call: a nil Job has no ID
// to attach the synthetic frames to, so we suppress rather than panic.
func TestExecuteOpt_RouterMissingNilJobNoPanic(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{
		Telemetry: rec,
	})

	// Must not panic.
	s.executeOpt(nil, false)

	if got := rec.endedCount(); got != 0 {
		t.Errorf("nil-j short-circuit must not emit; got %d ended events", got)
	}
}
