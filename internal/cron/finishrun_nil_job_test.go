package cron

import (
	"testing"
	"time"
)

// TestFinishRun_NilJob pins #837: a runCtx with no job must not panic the tick
// goroutine (robfig's Recover would swallow it above finishRun, leaving the
// gate stuck running=true). The guard finalizes the inflight gate and returns
// without a RunEnded — there is no job to key it on.
func TestFinishRun_NilJob(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, AllowNilRouter: true}, SchedulerDeps{Telemetry: rec})

	inf := &runInflight{}
	inf.running.Store(true)
	s.gateForTest().runningJobs.Store("job-nil-job-finalize", inf)

	s.finishRun(runCtx{jobID: "", runID: mustGenerateRunID(), startedAt: time.Now(), trigger: TriggerScheduled, finalizer: &runFinalizer{inflight: inf}},
		runOutcome{state: RunStateFailed, errClass: ErrClassSessionError, errMsg: "should never reach disk"})

	if got := rec.endedCount(); got != 0 {
		t.Errorf("nil-job finishRun must emit no ended event, got %d", got)
	}
	if inf.running.Load() {
		t.Error("nil-job guard must finalize the inflight gate (running should be false)")
	}
}
