package cron

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestR246GO3_FinishRunFinalizesInflightBeforeBroadcast pins the issue #689
// regression contract: when finishRun fires the cron_run_ended event, a
// concurrent CurrentRun(jobID) call MUST observe ok=false. Previously the
// defer's reset()+running.Store(false) ran after emitRunEnded, leaving a
// window where dashboard list requests saw runInflightView{Phase:Spawning}
// alongside the run-ended broadcast.
func TestR246GO3_FinishRunFinalizesInflightBeforeBroadcast(t *testing.T) {
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
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Simulate executeOpt's CAS-true window so we can drive finishRun in
	// isolation against a real *runInflight + per-run *runFinalizer.
	inflight := s.jobInflight(j.ID)
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
	s.finishRun(finishArgs{
		job:       j,
		runID:     "r-finalize",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		finalizer: finalizer,
	})

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

// TestR246GO3_OverlapSkippedDoesNotReleaseOwnerGate guards the contract that
// emitOverlapSkipped's finishRun call must NOT release the inflight gate
// held by the actually-running concurrent execution. A regression here
// would let two executeOpt invocations claim the gate in sequence and
// corrupt the in-flight metadata mid-run.
func TestR246GO3_OverlapSkippedDoesNotReleaseOwnerGate(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})

	j := &Job{ID: "job-overlap-noop", Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	inflight.populate(runInflightView{
		RunID: "r-owner",
		Phase: PhaseSending,
	})

	// emitOverlapSkipped goes through finishRun with finalizer=nil, so
	// finalize() short-circuits and the owner's metadata stays intact.
	s.emitOverlapSkipped(j, true)

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

// TestR246GO3_RunADeferDoesNotClobberRunBMetadata is the reviewer-found
// regression case: in production the executeOpt defer is gated behind
// deliverNotice (and other post-finishRun work), so a racing run-B that
// wins the next inflight CAS lands BEFORE run-A's defer fires. The
// per-run *runFinalizer design guarantees run-A's defer is a no-op
// (its own done=true) regardless of what run-B did to the shared
// *runInflight in the meantime — gate isolation comes from per-run
// finalizer identity, not from any atomic on *runInflight.
//
// This test fails on the original PR design (shared atomic.Bool released)
// and passes on the runFinalizer redesign. Sequence under test:
//  1. run-A: CAS true, populate "run-A" fields, finalizer-A.finalize() (via finishRun).
//  2. run-B (cron tick / TriggerNow): CAS true, populate "run-B" fields,
//     create its own finalizer-B.
//  3. run-A's late defer fires finalizer-A.finalize() — must be a no-op.
//  4. Verify run-B's RunID and running gate survive.
func TestR246GO3_RunADeferDoesNotClobberRunBMetadata(t *testing.T) {
	t.Parallel()
	inflight := &runInflight{}

	// run-A: CAS true, populate, finalize via "finishRun".
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("run-A CAS must succeed")
	}
	inflight.populate(runInflightView{RunID: "run-A"})
	finalizerA := &runFinalizer{inflight: inflight}
	finalizerA.finalize()
	if inflight.running.Load() {
		t.Fatal("finalizer-A must release running")
	}

	// run-B wins the next CAS and installs its own metadata + finalizer.
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("run-B CAS must succeed after run-A finalize")
	}
	inflight.populate(runInflightView{RunID: "run-B"})
	finalizerB := &runFinalizer{inflight: inflight}
	_ = finalizerB

	// run-A's late defer fires AFTER run-B has installed its fields. The
	// per-run finalizer's done=true short-circuits, leaving run-B alone.
	finalizerA.finalize()

	if !inflight.running.Load() {
		t.Error("run-A's late defer must not release run-B's running CAS")
	}
	if v, ok := inflight.snapshot(); !ok || v.RunID != "run-B" {
		t.Errorf("run-A's late defer must not clobber run-B's RunID: got %q ok=%v want %q", v.RunID, ok, "run-B")
	}
}

// TestRunFinalizerNilSafe pins the defensive nil receiver path. nil-safe
// short-circuit lets test fixtures and emitOverlapSkipped's finishRun
// path call through finalize() without conditionals at every call site.
func TestRunFinalizerNilSafe(t *testing.T) {
	t.Parallel()
	var f *runFinalizer
	f.finalize() // must not panic

	// Empty finalizer (nil inflight) is also a valid no-op.
	(&runFinalizer{}).finalize()
}
