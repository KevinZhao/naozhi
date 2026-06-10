package cron

import (
	"testing"
	"time"
)

// TestFinishRun_NilFinalizerNoPanic pins R20260527122801-CR-22 (#1337):
// emitOverlapSkipped (and emitSyntheticSkipped, the shared helper added
// for #1323) intentionally pass `finalizer: nil` to finishRun because the
// inflight gate they synthesise frames for belongs to a concurrently
// owning run that must NOT be released. finishRun calls
// a.finalizer.finalize() unconditionally — the nil-safe receiver path on
// (*runFinalizer).finalize() is what keeps this from panic'ing.
//
// This test covers the END-TO-END path (finishRun → finalizer.finalize)
// with a nil finalizer, complementing TestRunFinalizerNilSafe which only
// exercises the method itself. A future refactor that drops the nil-safe
// guard inside finalize() would pass that unit test (if rewritten with a
// non-nil typed pointer) but blow up here, surfacing the call-site
// contract violation immediately.
func TestFinishRun_NilFinalizerNoPanic(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{
		Telemetry: rec,
	})

	j := &Job{ID: "job-nil-finalizer", Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Direct finishRun call mirroring emitOverlapSkipped's finishArgs
	// literal (scheduler_finish.go's emitSyntheticSkipped) — finalizer
	// field is omitted (zero value = typed nil pointer).
	startedAt := time.Now()
	runID, err := generateRunID()
	if err != nil {
		t.Fatalf("generateRunID: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("finishRun panicked on nil finalizer: %v", r)
		}
	}()
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: startedAt, trigger: TriggerScheduled,
		state: RunStateSkipped, errClass: ErrClassOverlapSkipped,
		errMsg: "previous run still in flight", skipPersist: true,
		// finalizer: nil — the contract under test.
	})

	// And one ended event must still have landed on the broadcaster.
	if got := rec.endedCount(); got != 1 {
		t.Errorf("want 1 ended event, got %d", got)
	}
}
