package cron

// runScaffold is the lifecycle envelope every CAS-owning cron run body executes
// inside (executeOpt, #2174):
//
//  1. finalizer.finalize() runs in a defer, after any finalize the body
//     already performed via finishRun (idempotent).
//  2. When onPanic is set, a completed-guarded recover finalizes FIRST, then
//     calls onPanic: a concurrent CurrentRun(jobID) must never observe the run
//     inflight once cron_run_ended has been broadcast (#2094).
type runScaffold struct {
	// finalizer is never nil for a run that won the CAS; finalize() is nil-safe.
	finalizer *runFinalizer
	// jobID labels the panic log line (recordTriggerNowPanic).
	jobID string
	// onPanic, when non-nil, enables in-scaffold recovery: it runs only when body
	// did NOT complete, strictly after finalizer.finalize(), and must emit the
	// terminal broadcast + ended/failed counters the skipped finishRun would have.
	// nil means no recover(): the panic propagates to the caller's own boundary
	// (recordTriggerNowPanic for TriggerNow, robfig/cron's Recover for the tick, #1299).
	onPanic func(r any)
}

// run executes body inside the scaffold envelope. Do not reorder the two
// defers.
func (sc runScaffold) run(body func()) {
	defer sc.finalizer.finalize()
	// completed flips true only when body returns normally (it has already driven
	// finishRun → emitRunEnded), so a panic after a normal finish can never
	// double-emit an ended frame (#2064).
	completed := false
	defer func() {
		if sc.onPanic == nil {
			// No recover(): let the panic propagate to the caller's boundary.
			return
		}
		if r := recover(); r != nil {
			recordTriggerNowPanic(sc.jobID, r)
			if !completed {
				sc.finalizer.finalize()
				sc.onPanic(r)
			}
		}
	}()
	body()
	completed = true
}
