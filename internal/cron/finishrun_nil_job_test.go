package cron

import (
	"testing"
	"time"
)

// TestFinishRun_NilJobNoPanicNoEmit pins R243-ARCH-6 (#837): the terminal
// three-write protocol (recordTerminalResult → runStore.Append →
// emitRunEnded) dereferences a.job in every branch. A finishArgs literal
// carrying a nil job — a future call site mistake, or a snapshot path that
// left job unset — would panic the cron-tick goroutine. robfig's Recover
// wrapper swallows that panic ABOVE finishRun's frame, so the worst case is
// a RunStarted frame already broadcast with finalize()/emitRunEnded skipped,
// leaving the inflight gate stuck running=true and an orphaned "running"
// badge on the dashboard forever.
//
// The guard finalizes the (nil-safe) inflight gate and returns WITHOUT
// emitting a RunEnded — there is no job key to attach the ended frame to,
// so emitting a frame keyed on a nil job would itself panic. The contract
// under test: nil job ⇒ no panic AND no ended broadcast.
func TestFinishRun_NilJobNoPanicNoEmit(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
		Telemetry:      rec,
	})

	runID, err := generateRunID()
	if err != nil {
		t.Fatalf("generateRunID: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("finishRun panicked on nil job: %v", r)
		}
	}()
	s.finishRun(finishArgs{
		job:       nil, // the contract under test
		runID:     runID,
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateFailed,
		errClass:  ErrClassSessionError,
		errMsg:    "should never reach disk",
	})

	if got := rec.endedCount(); got != 0 {
		t.Errorf("nil-job finishRun must emit no ended event, got %d", got)
	}
}

// TestFinishRun_NilJobFinalizesInflight verifies the nil-job guard still
// finalizes the per-run inflight gate so a stuck running=true state is not
// left behind on the bail-out path. Without finalize() in the guard, a
// RunStarted-then-nil-job sequence would orphan the gate forever.
func TestFinishRun_NilJobFinalizesInflight(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
		Telemetry:      rec,
	})

	jobID := "job-nil-job-finalize"
	inf := &runInflight{}
	inf.running.Store(true)
	s.runningJobs.Store(jobID, inf)
	fin := &runFinalizer{inflight: inf}

	runID, err := generateRunID()
	if err != nil {
		t.Fatalf("generateRunID: %v", err)
	}
	s.finishRun(finishArgs{
		job:       nil,
		runID:     runID,
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateFailed,
		errClass:  ErrClassSessionError,
		finalizer: fin,
	})

	if inf.running.Load() {
		t.Error("nil-job guard must finalize the inflight gate (running should be false)")
	}
}
