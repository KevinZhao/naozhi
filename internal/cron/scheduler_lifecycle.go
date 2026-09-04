package cron

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// Stop halts the scheduler and saves state: it drains the cold-start GC
// (gcWaitBudget), then s.cron.Stop plus TriggerNow goroutines (one shared
// stopBudget), then always persists — worst case ~35s wall-clock, so a stuck
// job, webhook or trimAll cannot hold shutdown past systemd TimeoutStopSec.
//
// CONTRACT: Stop is terminal and the process exits shortly after. Three
// intentional-orphan goroutine sites may outlive the budget by design: the
// triggerWG.Wait and gcWG.Wait drain wrappers (WaitGroup.Wait has no cancel
// signal) and runDeadlineWatchdog's parked interrupt (#498, #1072). Any
// Start-after-Stop reuse path MUST first make them ctx-aware (see #984).
func (s *Scheduler) Stop() {
	// context.Background() rather than nil: both yield a nil Done channel via
	// ctxDone, so the drain helpers' optional cancel arm stays inert; the
	// nil-ctx path itself remains supported for external callers.
	s.stopWithCtx(context.Background())
}

// StopContext is the idiomatic Stop(ctx) entry point: callers thread a
// shutdown-scoped context (e.g. derived from systemd's TimeoutStopSec) so the
// drain phases short-circuit on ctx cancellation instead of waiting out their
// internal budgets (#1168). ctx is advisory and additive — nil behaves exactly
// like Stop(); when ctx fires each drain wait returns with the same Warn +
// budget counter it logs on its own deadline. persistOnShutdown ALWAYS runs,
// even on a cancelled ctx, so mutations that already returned 2xx are kept.
func (s *Scheduler) StopContext(ctx context.Context) {
	s.stopWithCtx(ctx)
}

// stopWithCtx is the shared body of Stop / StopContext. ctx may be nil
// (Stop's path) — the drain helpers treat a nil ctx as "no extra cancel".
func (s *Scheduler) stopWithCtx(ctx context.Context) {
	// CAS guard makes Stop idempotent (mirrors Start's `started` CAS): repeat
	// calls must not re-run the timer-allocating drains or double-run the
	// final persist. stopCancel is itself idempotent.
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	s.stopCancel()

	// Each shutdown stage owns an explicit budget and logs a Warn + bumps its
	// CronStopBudgetExceeded* counter on its own deadline; Stop itself
	// contains no budget arithmetic.
	s.waitGCDrain(ctx)
	deadlineHit, stopStart := s.drainCronStop(ctx)
	if !deadlineHit {
		s.drainTriggerWG(ctx, stopStart)
	}
	s.persistOnShutdown()
}

// ctxDone returns ctx.Done() or a nil channel when ctx is nil. A receive from
// a nil channel blocks forever, so a nil-ctx select case is inert — letting
// the drain helpers add an optional ctx-cancel arm without branching.
func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// waitGCDrain blocks until the cold-start GC goroutine spawned in Start()
// completes or gcWaitBudget elapses. trimAll's filesystem mutations on the
// runs/ tree would race the upcoming persist + Append-from-triggerWG paths
// without this drain; the budget keeps a wedged trimAll from pinning systemd
// TimeoutStopSec.
func (s *Scheduler) waitGCDrain(ctx context.Context) {
	gcDone := make(chan struct{})
	go func() {
		s.gcWG.Wait()
		close(gcDone)
	}()
	gcTimer := time.NewTimer(gcWaitBudget)
	defer gcTimer.Stop()
	select {
	case <-gcDone:
	case <-ctxDone(ctx):
		// The caller's shutdown ctx pre-empts the internal budget; account it
		// as a budget breach so dashboards alert identically (#1168).
		metrics.CronStopBudgetExceededGCTotal.Add(1)
		slog.Warn("cron: gc goroutine wait cancelled by stop ctx", "budget", gcWaitBudget)
	case <-gcTimer.C:
		// Counter pairs the Warn so dashboards can alert on shutdown-budget
		// breaches without grepping journalctl (#1083).
		metrics.CronStopBudgetExceededGCTotal.Add(1)
		slog.Warn("cron: gc goroutine wait timeout", "budget", gcWaitBudget)
	}
}

// drainCronStop signals the robfig/cron runner to stop accepting new ticks
// and waits up to stopBudget for in-flight ticks to drain. Returns
// (deadlineHit, stopStart) — the caller skips drainTriggerWG when deadlineHit
// is true (the budget is shared across both phases); stopStart anchors the
// remaining-budget arithmetic in drainTriggerWG.
func (s *Scheduler) drainCronStop(ctx context.Context) (deadlineHit bool, stopStart time.Time) {
	cronDoneCtx := s.cron.Stop()

	// Single overall deadline shared across both waits: if cron.Stop drains
	// fast the full budget remains for triggerWG; if it eats the budget we
	// skip triggerWG.Wait entirely. Either way saveJobs runs. stopStart lets
	// drainTriggerWG derive the remaining budget with a fresh timer instead
	// of reusing this (possibly fired) timer channel across two selects.
	stopStart = time.Now()
	// Zero-value field = hand-constructed *Scheduler (tests); fall back to
	// the const, never a package var (#947, #1712).
	budget := s.stopBudget
	if budget <= 0 {
		budget = defaultStopBudget
	}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()

	select {
	case <-cronDoneCtx.Done():
	case <-ctxDone(ctx):
		// Shutdown ctx pre-empts the drain budget; deadlineHit makes the
		// caller skip drainTriggerWG so the single-ceiling contract holds (#1168).
		deadlineHit = true
		metrics.CronStopBudgetExceededDrainTotal.Add(1)
		slog.Warn("cron scheduler: stop cancelled by ctx before cron.Stop drained, proceeding",
			"budget", budget)
	case <-deadline.C:
		deadlineHit = true
		// see #1083 (counter pairs the Warn)
		metrics.CronStopBudgetExceededDrainTotal.Add(1)
		slog.Warn("cron scheduler: stop deadline exceeded before cron.Stop drained, proceeding",
			"budget", budget)
	}
	return deadlineHit, stopStart
}

// drainTriggerWG waits for TriggerNow + deliverNotice goroutines to drain,
// budgeted by the *remaining* share of stopBudget after drainCronStop. The
// caller must skip this phase when drainCronStop's deadlineHit is true so the
// budget stays a single overall ceiling.
//
// When the deadline pre-empts triggerDone, the `go func() { triggerWG.Wait();
// close(...) }` wrapper stays parked — the intentional-orphan path in the Stop
// CONTRACT (#606). No cancel/reclaim path is added: WaitGroup.Wait has no
// cancel signal and Stop is terminal. notifyTarget's replyCtx already chains
// to stopCtx (#799); this deadline is the backstop for Background-parented paths.
func (s *Scheduler) drainTriggerWG(ctx context.Context, stopStart time.Time) {
	triggerDone := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(triggerDone)
	}()
	// Derive the remaining budget from stopStart. If cron.Stop drained at the
	// budget's edge, remaining can be ~0; clamp to a tiny floor so an
	// already-closed triggerDone is still observed rather than wedging on a
	// 0-duration timer. NewTimer + defer Stop (not time.After) releases the
	// timer slot deterministically on the fast path. Same budget read as drainCronStop.
	budget := s.stopBudget
	if budget <= 0 {
		budget = defaultStopBudget
	}
	remaining := budget - time.Since(stopStart)
	if remaining < time.Millisecond {
		remaining = time.Millisecond
	}
	triggerTimer := time.NewTimer(remaining)
	defer triggerTimer.Stop()
	select {
	case <-triggerDone:
	case <-ctxDone(ctx):
		// Shutdown ctx pre-empts the remaining budget (#1168).
		metrics.CronStopBudgetExceededTriggerTotal.Add(1)
		slog.Warn("cron scheduler: stop cancelled by ctx during triggerWG wait, proceeding",
			"budget", budget, "remaining_ms", remaining.Milliseconds())
	case <-triggerTimer.C:
		// see #1083 (counter pairs the Warn)
		metrics.CronStopBudgetExceededTriggerTotal.Add(1)
		slog.Warn("cron scheduler: stop deadline exceeded during triggerWG wait, proceeding",
			"budget", budget, "remaining_ms", remaining.Milliseconds())
	}
}

// persistOnShutdown runs the final cron_jobs.json write through
// persistJobsLocked + the saveSeq gate, so a queued-but-not-landed mutator
// save cannot later overwrite Stop's snapshot with stale data. Failures are
// tagged persist=FAILED_DURING_SHUTDOWN (#690): unlike the per-mutation save
// there is no "next save" to retry, so this is the unrecoverable-data-loss
// alert. saveMarshaledSeq returns void and Stores lastSavedSeq only on
// success, so a write failure is detected by comparing lastSavedSeq against
// the seq we queued; a newer save racing ahead also advances it, which is
// correctly treated as success (#1301).
func (s *Scheduler) persistOnShutdown() {
	s.mu.Lock()
	save, err := s.persistJobsLocked()
	// Read the seq we just queued so the post-save check below is
	// deterministic (saveSeq.Add was the last mutation persistJobsLocked
	// performed before returning).
	queuedSeq := s.saveSeq.Load()
	s.mu.Unlock()
	if err != nil {
		slog.Error("marshal cron store on shutdown",
			"err", err,
			"persist", "FAILED_DURING_SHUTDOWN")
		return
	}
	if save == nil {
		return
	}
	save()
	// After save() returns, lastSavedSeq has either advanced to >= queuedSeq
	// (success or a newer save raced ahead) or it has not (WriteFileAtomic
	// failed AND no newer save took our place — disk lags memory at exit).
	if landed := s.lastSavedSeq.Load(); landed < queuedSeq {
		slog.Error("save cron store on shutdown failed; in-memory state will not survive restart",
			"queued_seq", queuedSeq,
			"last_saved_seq", landed,
			"persist", "FAILED_DURING_SHUTDOWN")
	}
}
