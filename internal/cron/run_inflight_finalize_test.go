package cron

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestFinishRunFinalizesInflightBeforeBroadcast pins the issue #689
// regression contract: when finishRun fires the cron_run_ended event, a
// concurrent CurrentRun(jobID) call MUST observe ok=false. Previously the
// defer's reset()+running.Store(false) ran after emitRunEnded, leaving a
// window where dashboard list requests saw runInflightView{Phase:Spawning}
// alongside the run-ended broadcast.
func TestFinishRunFinalizesInflightBeforeBroadcast(t *testing.T) {
	t.Parallel()

	// Capture CurrentRun visibility from inside the broadcaster callback —
	// the exact concurrency window the issue describes. Phase D (RFC §3.5)
	// migrated SetOnRunEnded to runtelemetry.Broadcaster; observingBroadcaster
	// (defined in finishrun_persist_before_emit_test.go) wraps the
	// callback into the new shape.
	var sawOK atomic.Bool
	var sawPhase atomic.Pointer[string]
	sawOK.Store(true)

	var sched *Scheduler
	deps := SchedulerDeps{Router: &fakeRouter{}, Telemetry: observingBroadcaster{onEnded: func() {
		v, ok := sched.CurrentRun("job-finalize")
		sawOK.Store(ok)
		phase := v.Phase
		sawPhase.Store(&phase)
	}}}
	sched = NewScheduler(SchedulerConfig{MaxJobs: 5}, deps)
	s := sched

	j := &Job{ID: "job-finalize", Schedule: "@every 5m"}
	s.putJobForTest(j)

	// Simulate executeOpt's CAS-true window so we can drive finishRun in
	// isolation against a real *runInflight + per-run *runFinalizer.
	inflight := s.gateForTest().jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	inflight.populate(runInflightView{
		RunID:     "r-finalize",
		StartedAt: time.Now(),
		Phase:     PhaseSpawning,
		Trigger:   TriggerScheduled,
	})

	if v, ok := inflight.snapshot(); !ok || v.Phase != PhaseSpawning {
		t.Fatalf("pre-finish snapshot want Spawning/ok=true, got %+v ok=%v", v, ok)
	}

	finalizer := &runFinalizer{inflight: inflight}
	s.finishRun(runCtx{jobID: j.ID, runID: "r-finalize", startedAt: time.Now(), trigger: TriggerScheduled, finalizer: finalizer}, runOutcome{state: RunStateSucceeded})

	if sawOK.Load() {
		phase := ""
		if p := sawPhase.Load(); p != nil {
			phase = *p
		}
		t.Fatalf("CurrentRun must return ok=false during cron_run_ended (got phase=%q)", phase)
	}

	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("inflight.running must be released after finishRun → finalize")
	}
	inflight.running.Store(false)
}

// TestOverlapSkippedDoesNotReleaseOwnerGate guards the contract that
// emitOverlapSkipped's finishRun call must NOT release the inflight gate
// held by the actually-running concurrent execution. A regression here
// would let two executeOpt invocations claim the gate in sequence and
// corrupt the in-flight metadata mid-run.
func TestOverlapSkippedDoesNotReleaseOwnerGate(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})

	j := &Job{ID: "job-overlap-noop", Schedule: "@every 5m"}
	s.putJobForTest(j)

	inflight := s.gateForTest().jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	inflight.populate(runInflightView{
		RunID: "r-owner",
		Phase: PhaseSending,
	})

	// emitOverlapSkipped goes through finishRun with finalizer=nil, so
	// finalize() short-circuits and the owner's metadata stays intact.
	s.emitOverlapSkipped(j.ID, true)

	v, ok := inflight.snapshot()
	if !ok {
		t.Fatal("owner inflight must remain ok=true after overlap-skipped path")
	}
	if v.RunID != "r-owner" {
		t.Errorf("owner RunID clobbered: got %q want %q", v.RunID, "r-owner")
	}
	if v.Phase != PhaseSending {
		t.Errorf("owner Phase clobbered: got %q want %q", v.Phase, PhaseSending)
	}

	// Cleanup as the real owner's defer would.
	(&runFinalizer{inflight: inflight}).finalize()
}
