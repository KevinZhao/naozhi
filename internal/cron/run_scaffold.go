package cron

import "github.com/naozhi/naozhi/internal/metrics"

// runScaffold is the lifecycle envelope every CAS-owning cron run body executes
// inside. It is the single home of the run-goroutine teardown contract that
// executeOpt (TriggerNow / scheduled tick) and dispatchReplay (sandbox replay)
// used to hand-copy (R202606-ARCH-7, #2174):
//
//  1. metrics.CronRunInflight is +1'd immediately before body and -1'd in a
//     defer, so the gauge pairs on every exit path (R242-CR-14 / #706).
//  2. finalizer.finalize() runs in that same defer, AFTER any finalize the body
//     itself performed via finishRun (idempotent, R246-GO-3 / #689). The
//     reset → CAS-release ordering (R238-GO-2) lives inside finalize().
//  3. When onPanic is set, a completed-guarded recover closes the lifecycle
//     of a body that panicked before reaching its terminal finishRun: log,
//     finalize FIRST, then onPanic (broadcast + counters). Defers run LIFO,
//     so this recover fires before the outer defer — finalizing here is what
//     keeps the finalize-before-broadcast contract (R20260614-032346-LB-replay
//     / #2094): a concurrent CurrentRun(jobID) must never observe the run
//     inflight once a cron_run_ended frame has been broadcast.
//
// The caller owns admission (CAS win) and, when it spawned a goroutine, the
// matching triggerWG.Done — both happen outside the scaffold because
// executeOpt is invoked synchronously inside the TriggerNow / tick goroutine
// and only owns the run from CAS-win onward.
type runScaffold struct {
	// finalizer is the per-run stack-local cleanup hook (never nil for a run
	// that won the CAS; finalize() itself is nil-safe).
	finalizer *runFinalizer
	// jobID labels the panic log line (recordTriggerNowPanic).
	jobID string
	// onPanic, when non-nil, enables in-scaffold recovery. It runs only when
	// body did NOT complete, strictly after finalizer.finalize(), and is
	// expected to emit the terminal broadcast and bump the ended/failed
	// counters the skipped finishRun would have bumped.
	//
	// nil means the scaffold does NOT recover: the finalize + gauge defer
	// still fires, then the panic propagates to the caller's own boundary
	// (executeIfNotDeletedOrPaused's recordTriggerNowPanic recover for
	// TriggerNow, robfig/cron's Recover chain for the scheduled tick — see
	// R20260527-COR-7 / #1299 in scheduler_jobs.go). Installing a recover
	// here for that path would silently swallow the panic before robfig's
	// logger sees it, changing the tick path's failure signal.
	onPanic func(r any)
}

// run executes body inside the scaffold envelope. See the type doc for the
// exact defer ordering; do not reorder the two defers or move the gauge +1
// below anything that can return early.
func (sc runScaffold) run(body func()) {
	defer func() {
		// Nested defer: the gauge -1 must land even if finalize() itself
		// panics, otherwise CronRunInflight drifts +1 forever. The panic is
		// NOT swallowed here — it still reaches the caller's boundary.
		defer metrics.CronRunInflight.Add(-1)
		sc.finalizer.finalize()
	}()
	// completed flips true only when body returns normally — by then it has
	// already driven finishRun → emitRunEnded itself. Guarding on it means a
	// (practically impossible) panic after a normal finish can never
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
	metrics.CronRunInflight.Add(1)
	body()
	completed = true
}
