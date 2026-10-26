package cron

import "testing"

// newScaffoldFixture returns an inflight gate that has "won" the CAS (the
// precondition every runScaffold caller establishes) plus its finalizer.
func newScaffoldFixture() (*runInflight, *runFinalizer) {
	inflight := &runInflight{}
	inflight.running.Store(true)
	inflight.populate(runInflightView{RunID: "r1", Phase: PhaseQueued})
	return inflight, &runFinalizer{inflight: inflight}
}

// TestRunScaffold_NormalPath pins the happy-path envelope (#2174): finalize
// released the CAS gate exactly once, and onPanic never fires.
func TestRunScaffold_NormalPath(t *testing.T) {
	inflight, finalizer := newScaffoldFixture()

	bodyCalls, panicCalls := 0, 0
	runScaffold{
		finalizer: finalizer,
		jobID:     "job-normal",
		onPanic:   func(any) { panicCalls++ },
	}.run(func() {
		bodyCalls++
		if finalizer.done {
			t.Error("finalizer must not be finalized while body is running")
		}
	})

	if bodyCalls != 1 {
		t.Fatalf("body ran %d times, want 1", bodyCalls)
	}
	if !finalizer.done {
		t.Error("finalizer.finalize() must have run in the scaffold defer")
	}
	if inflight.running.Load() {
		t.Error("CAS gate still held after normal completion")
	}
	if _, ok := inflight.snapshot(); ok {
		t.Error("inflight view still populated after finalize")
	}
	if panicCalls != 0 {
		t.Errorf("onPanic fired %d times on the normal path, want 0", panicCalls)
	}
}

// TestRunScaffold_BodyFinalizedViaFinishRun mirrors the real success path:
// finishRun already called finalize() inside body. The scaffold defer's
// finalize must then be an idempotent no-op (finalizer.done already true) —
// pinning that the scaffold never double-resets a slot a later run may have
// re-acquired (R246-GO-3 / #689).
func TestRunScaffold_BodyFinalizedViaFinishRun(t *testing.T) {
	inflight, finalizer := newScaffoldFixture()

	runScaffold{finalizer: finalizer, jobID: "job-prefinalized"}.run(func() {
		finalizer.finalize()
		// Simulate run-B winning the freed slot before run-A's defer fires.
		inflight.running.Store(true)
		inflight.populate(runInflightView{RunID: "r2", Phase: PhaseQueued})
	})

	if !inflight.running.Load() {
		t.Error("scaffold defer clobbered run-B's CAS gate; finalize must be idempotent per finalizer")
	}
	if v, ok := inflight.snapshot(); !ok || v.RunID != "r2" {
		t.Errorf("scaffold defer clobbered run-B's inflight view: got %+v ok=%v", v, ok)
	}
}

// TestRunScaffold_PanicWithHook pins the replay-style panic path (#2064 /
// #2094): the panic is recovered, onPanic fires exactly once, and at the
// instant onPanic runs the slot is ALREADY finalized (finalize-before-
// broadcast).
func TestRunScaffold_PanicWithHook(t *testing.T) {
	inflight, finalizer := newScaffoldFixture()

	panicCalls := 0
	var gotPanic any
	var finalizedAtHook, gateHeldAtHook bool

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic escaped the scaffold despite onPanic being set: %v", r)
			}
		}()
		runScaffold{
			finalizer: finalizer,
			jobID:     "job-panic",
			onPanic: func(r any) {
				panicCalls++
				gotPanic = r
				finalizedAtHook = finalizer.done
				gateHeldAtHook = inflight.running.Load()
			},
		}.run(func() {
			panic("boom in body")
		})
	}()

	if panicCalls != 1 {
		t.Fatalf("onPanic fired %d times, want exactly 1", panicCalls)
	}
	if gotPanic != "boom in body" {
		t.Errorf("onPanic received %v, want the body's panic value", gotPanic)
	}
	if !finalizedAtHook {
		t.Error("onPanic ran before finalizer.finalize(): finalize-before-broadcast contract violated (#2094)")
	}
	if gateHeldAtHook {
		t.Error("CAS gate still held when onPanic ran; a concurrent CurrentRun could observe the run inflight after cron_run_ended")
	}
	if !finalizer.done || inflight.running.Load() {
		t.Error("slot not released after panic path")
	}
}

// TestRunScaffold_PanicWithoutHookPropagates pins the executeOpt-style
// contract: with onPanic == nil the scaffold does NOT recover — the panic
// reaches the caller's own boundary (executeIfNotDeletedOrPaused / robfig
// Recover) — but the finalize defer still fires first, so the slot is
// released by the time the caller sees the panic.
func TestRunScaffold_PanicWithoutHookPropagates(t *testing.T) {
	inflight, finalizer := newScaffoldFixture()

	var caught any
	var finalizedAtCatch, gateHeldAtCatch bool
	func() {
		defer func() {
			caught = recover()
			finalizedAtCatch = finalizer.done
			gateHeldAtCatch = inflight.running.Load()
		}()
		runScaffold{finalizer: finalizer, jobID: "job-propagate"}.run(func() {
			panic("boom propagates")
		})
	}()

	if caught != "boom propagates" {
		t.Fatalf("caller recovered %v, want the body's panic to propagate when onPanic is nil", caught)
	}
	if !finalizedAtCatch {
		t.Error("finalizer must be finalized before the panic reaches the caller")
	}
	if gateHeldAtCatch {
		t.Error("CAS gate leaked across a propagated panic")
	}
}

// TestRunScaffold_NilFinalizerSafe: finalize() is documented nil-safe; the
// scaffold must not NPE on a nil finalizer (defensive — every real caller
// passes a non-nil one).
func TestRunScaffold_NilFinalizerSafe(t *testing.T) {
	ran := false
	runScaffold{jobID: "job-nil"}.run(func() { ran = true })
	if !ran {
		t.Fatal("body did not run")
	}
}
