package cron

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// Stop halts the scheduler and saves state. It waits for both scheduled jobs
// (drained by s.cron.Stop) and any TriggerNow-spawned goroutines before
// returning, so callers can safely tear down the router afterwards.
//
// Shutdown wall-clock is bounded by gcWaitBudget + stopBudget (5s + 30s
// = 35s default; both are independent timers — gcWG.Wait runs first to
// completion or timeout, *then* the stopBudget deadline starts for
// cron.Stop + triggerWG). R247-GO-14: prior godoc only mentioned the 30s
// stopBudget, which under-stated worst-case Stop() wall-clock by the
// 5s gc wait when the cold-start GC goroutine is wedged on a stuck
// filesystem.
//
// A stuck cron job (execute() hanging past ctx cancel, e.g. a broken
// shim ignoring context) or a stuck triggerWG (deliverNotice → platform
// Reply webhook that refuses to honour its own timeout) cannot hold us
// past stopBudget. A stuck trimAll cannot hold us past gcWaitBudget. The
// final saveJobs runs regardless so a stuck drain does not cost the
// state file. Tests overriding budgets via WithStopBudgetField on the
// *Scheduler instance see the same composition.
//
// CONTRACT: Stop assumes the naozhi process terminates shortly after it
// returns. When triggerWG.Wait is cut off by the budget, the wrapper
// goroutine around it is intentionally leaked — the deliverNotice that
// holds it is typically blocked on a hung platform webhook, and the only
// way to reclaim it is to let the OS tear the process down. This is
// acceptable precisely because Scheduler is not reusable: there is no
// path that calls Stop() and then constructs new cron work on the same
// instance. If you ever add one, you MUST replace the bare wrapper with
// a ctx-aware pattern and reclaim the goroutine, otherwise restart
// cycles accumulate stuck webhook goroutines until OOM. R44-REL-
// TRIGGER-GOROUTINE.
//
// The same intentional-orphan contract applies to the gcWG.Wait wrapper
// goroutine spawned just below (the cold-start GC waiter). When
// gcWaitBudget elapses with trimAll still wedged on a stuck filesystem
// (ReadDir / Remove not returning), the wrapper around `gcWG.Wait()`
// stays parked and is reclaimed only when the OS tears the process
// down. Rationale is identical: Scheduler is single-shot, the process
// is moments from exit, and gcWG offers no cancel signal. If a future
// reuse path is added (Start after Stop on the same instance), this
// wrapper MUST also be migrated to a ctx-aware pattern (e.g. trimAll
// observing stopCtx) so successive lifecycles do not accumulate stuck
// filesystem-IO goroutines until OOM. R247-GO-7.
//
// THIRD intentional-orphan site (R250-GO-9 / #1072): runDeadlineWatchdog
// in scheduler_run.go spawns a goroutine that parks on the run's sendCtx
// outside the triggerWG accounting. On the success path the caller's
// sendCancel() unblocks <-ctx.Done() and the goroutine returns; on the
// stuck-Send path (CLI ignoring ctx, shim hanging) the watchdog stays
// parked until Send eventually returns or the OS reclaims it. The
// goroutine holds only the abortCh send (buffer=1, so the send itself
// does not block) — no triggerWG.Add is held, so Stop()'s budget never
// waits on it. Acceptable on the same single-shot-Scheduler grounds as
// triggerWG / gcWG, with one extra leak source operators should know
// exists when reading "deadline fired but interrupt did not land"
// post-Stop log lines. If a future reuse path is added the watchdog
// MUST also be migrated under triggerWG so its lifetime is bounded by
// the same stopBudget the Send-spawning code is.
func (s *Scheduler) Stop() {
	s.stopWithCtx(nil)
}

// StopContext is the idiomatic Stop(ctx) entry point (golang/go#36363):
// callers thread a shutdown-scoped context — e.g. one derived from systemd's
// TimeoutStopSec — so the drain phases short-circuit on ctx cancellation
// instead of waiting out their internal budgets. This complements
// StartContext(ctx) so the lifecycle reads Start(ctx) / Stop(ctx) rather than
// stashing a ParentCtx field.
//
// R250-ARCH-5 (#1168): ctx is advisory and additive. A nil ctx behaves exactly
// like Stop() (each phase honours only its internal per-phase budget). When
// ctx fires, each drain wait returns promptly with the same Warn + budget
// counter it logs on its own deadline; the orphaned wrapper goroutines die
// with the process exactly as the Stop() CONTRACT block documents. The final
// persistOnShutdown ALWAYS runs (even on a cancelled ctx) so mutations that
// already returned 2xx are never lost.
func (s *Scheduler) StopContext(ctx context.Context) {
	s.stopWithCtx(ctx)
}

// stopWithCtx is the shared body of Stop / StopContext. ctx may be nil
// (Stop's path) — the drain helpers treat a nil ctx as "no extra cancel".
func (s *Scheduler) stopWithCtx(ctx context.Context) {
	// R20260526-GO-007: idempotent CAS guard. Without this, repeat calls
	// re-enter the timer-allocating + persist branches below — wasting
	// time.NewTimer slots, double-running persistJobsLocked, and racing the
	// final marshaled write against itself. Mirror Start()'s `started`
	// CAS so the lifecycle is symmetrically idempotent. stopCancel is
	// already idempotent (context cancel is a no-op after the first call),
	// so callers that bypass this guard via earlier wiring are unaffected.
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	s.stopCancel()

	// R247-CR-4 (#584): the four shutdown stages each own an explicit
	// budget; Stop() orchestrates them in order. Each helper logs a Warn +
	// bumps its CronStopBudgetExceeded* counter on its own deadline; Stop
	// itself contains no budget arithmetic.
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
// completes or gcWaitBudget elapses. Filesystem mutations on the runs/
// tree from trimAll race the upcoming persist + Append-from-triggerWG
// paths if we don't drain first; the budget keeps a wedged trimAll from
// pinning systemd TimeoutStopSec. R236-GO-01 (origin) / R247-CR-4 (extract).
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
		// R250-ARCH-5 (#1168): caller's shutdown ctx pre-empts the internal
		// budget. Account it like a budget breach so dashboards alert
		// identically whether the cap came from gcWaitBudget or the
		// operator's TimeoutStopSec ctx.
		metrics.CronStopBudgetExceededGCTotal.Add(1)
		slog.Warn("cron: gc goroutine wait cancelled by stop ctx", "budget", gcWaitBudget)
	case <-gcTimer.C:
		// R250-GO-20 (#1083): pair the per-phase Warn with a counter so
		// dashboards can alert on shutdown-budget breaches without
		// grepping journalctl. Useful for catching systemd TimeoutStopSec
		// proximity in production.
		metrics.CronStopBudgetExceededGCTotal.Add(1)
		slog.Warn("cron: gc goroutine wait timeout", "budget", gcWaitBudget)
	}
}

// drainCronStop signals the robfig/cron runner to stop accepting new ticks
// and waits up to stopBudget for in-flight ticks to drain. Returns
// (deadlineHit, stopStart) — caller skips drainTriggerWG when deadlineHit
// is true (the budget is shared across both phases). stopStart anchors the
// remaining-budget arithmetic in drainTriggerWG so both phases account
// against the same wall clock. R246-GO-13 / R247-CR-4.
func (s *Scheduler) drainCronStop(ctx context.Context) (deadlineHit bool, stopStart time.Time) {
	cronDoneCtx := s.cron.Stop()

	// Single overall deadline shared across both waits. If cron.Stop drains
	// fast we have the full budget for triggerWG; if it eats the budget we
	// skip triggerWG.Wait entirely (the leaked goroutines die when the
	// process exits moments later). Either way saveJobs runs — losing it
	// would undo mutations that had already returned 2xx to dashboard/IM.
	//
	// R246-GO-13: track stopStart and re-derive the remaining budget for
	// the second select via time.After, instead of reusing deadline.C from
	// a NewTimer across two select statements. Reusing a fired timer's
	// channel is a known footgun (the receive cannot be guaranteed to
	// observe the prior firing exactly once across both selects, and Go
	// makes no documented guarantee about timer-channel buffering across
	// independent receivers); a fresh time.After on the remaining budget
	// is the explicit, documented pattern. The first select still uses
	// the NewTimer so we can defer-Stop it on the early-drain path.
	stopStart = time.Now()
	// R249-CR-3 (#947): read the per-instance budget. Falls back to the
	// const default when the field is the zero value — e.g. tests that
	// hand-construct *Scheduler without going through NewScheduler.
	// R20260603150052-GO-2 (#1712): fall back to the defaultStopBudget
	// const, NOT a package-level var. NewScheduler now seeds the field
	// from the const directly, so the only var swap (test budget
	// injection) lives on the per-instance field via WithStopBudgetField;
	// reading a package var here would re-race a concurrent Stop on a
	// hand-built Scheduler against that swap.
	budget := s.stopBudget
	if budget <= 0 {
		budget = defaultStopBudget
	}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()

	select {
	case <-cronDoneCtx.Done():
	case <-ctxDone(ctx):
		// R250-ARCH-5 (#1168): shutdown ctx pre-empts the drain budget. Set
		// deadlineHit so the caller skips drainTriggerWG — the single overall
		// ceiling contract (one budget across both phases) extends to the ctx
		// cancel edge the same way the timer deadline does.
		deadlineHit = true
		metrics.CronStopBudgetExceededDrainTotal.Add(1)
		slog.Warn("cron scheduler: stop cancelled by ctx before cron.Stop drained, proceeding",
			"budget", budget)
	case <-deadline.C:
		deadlineHit = true
		// R250-GO-20 (#1083): see GC counter rationale above.
		metrics.CronStopBudgetExceededDrainTotal.Add(1)
		slog.Warn("cron scheduler: stop deadline exceeded before cron.Stop drained, proceeding",
			"budget", budget)
	}
	return deadlineHit, stopStart
}

// drainTriggerWG waits for TriggerNow + deliverNotice goroutines to drain,
// budgeted by the *remaining* share of stopBudget after drainCronStop. Caller
// must skip this phase entirely when drainCronStop's deadlineHit is true so
// the budget is honoured as a single overall ceiling.
//
// R222-GO-10 / R217-GO-5 / R44 (#606): when the deadline pre-empts
// triggerDone, the wrapper goroutine started by
// `go func() { s.triggerWG.Wait(); close(...) }` stays parked on
// triggerWG.Wait — exactly the intentional-orphan path documented in the
// Stop CONTRACT block. Reclaim happens when the OS tears the process down.
// We deliberately do NOT add a sync.Once / chan-cancel reclaim path here:
// triggerWG.Wait does not accept a cancel signal, and Scheduler is
// single-shot (Stop is terminal). A goroutine-leak detector running in
// tests that shorten stopBudget to milliseconds will surface this orphan;
// tests that care should plumb a non-stuck deliverNotice fake instead.
// #606 is the [REPEAT-N] reaffirm of this design — keeping the issue
// reference here so future reviewers see the lineage at a glance.
//
// Bound triggerWG.Wait with the *remaining* share of the same budget:
// manual TriggerNow respects stopCtx via execute(), and R243-SEC-14
// (#799) wired notifyTarget's replyCtx to s.stopCtx so a hung webhook
// short-circuits on the cancel edge instead of waiting for its own
// per-target timer. The deadline here remains the backstop for any
// notify path that still parents on Background (e.g. a future helper
// or a test fake that bypasses notifyTarget): without it a stuck
// platform HTTP call could otherwise pin Stop() past systemd
// TimeoutStopSec.
//
// R247-CR-4: extracted from Stop().
func (s *Scheduler) drainTriggerWG(ctx context.Context, stopStart time.Time) {
	triggerDone := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(triggerDone)
	}()
	// R246-GO-13: derive remaining budget from stopStart instead of
	// re-reading deadline.C. If cron.Stop drained at the very edge of
	// the budget, remaining can be near-zero; clamp to a tiny floor so
	// we still observe an instantaneous triggerDone (already-closed
	// channel) without wedging on a 0-duration timer. The clamp is
	// not a guaranteed minimum wait — both the channel and the timer
	// are checked in the same select.
	//
	// R249-GO-4: use NewTimer + defer Stop instead of time.After.
	// time.After returns a fresh timer whose underlying resources
	// are released only when it fires; on the triggerDone-fast path
	// the timer would leak its slot until expiry (~30s default).
	// More urgently, with remaining clamped to 1ms the timer almost
	// certainly fires before the select runs, and a fired channel
	// from time.After is unreachable for explicit Stop. Mirror the
	// first select's NewTimer + defer Stop pattern (line ~820) so
	// both halves of Stop release timer state deterministically.
	// R249-CR-3 (#947): same per-instance budget read as drainCronStop;
	// must match — keeping a single overall ceiling across both halves
	// of Stop is the contract drainCronStop's godoc pins.
	// R20260603150052-GO-2 (#1712): fall back to the defaultStopBudget
	// const, mirroring drainCronStop — never the removed package var.
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
		// R250-ARCH-5 (#1168): shutdown ctx pre-empts the remaining budget.
		metrics.CronStopBudgetExceededTriggerTotal.Add(1)
		slog.Warn("cron scheduler: stop cancelled by ctx during triggerWG wait, proceeding",
			"budget", budget, "remaining_ms", remaining.Milliseconds())
	case <-triggerTimer.C:
		// R250-GO-20 (#1083): see GC counter rationale above.
		metrics.CronStopBudgetExceededTriggerTotal.Add(1)
		slog.Warn("cron scheduler: stop deadline exceeded during triggerWG wait, proceeding",
			"budget", budget, "remaining_ms", remaining.Milliseconds())
	}
}

// persistOnShutdown runs the final cron_jobs.json write through
// persistJobsLocked + saveSeq gate. Routing through the gate (not a bare
// WriteFileAtomic) keeps a queued-but-not-landed mutator save from later
// overwriting Stop's snapshot with stale data — R232-ARCH-10. R246-GO-5
// (#690) tags failures with persist=FAILED_DURING_SHUTDOWN so log
// aggregation routes them to the unrecoverable-data-loss alert channel
// (the per-mutation "save cron store" failure is recoverable on the next
// mutation; this one is not, the process is moments from exit).
//
// R247-CR-4: extracted from Stop().
//
// R20260527-GO-12 (#1301): saveMarshaledSeq returns void; the WriteFileAtomic
// failure inside is logged at slog.Error("save cron store") but never
// reaches the caller. The mutation hot path tolerates that (the next save
// retries naturally) but on shutdown there is no "next save" — disk state
// silently lags in-memory state and operators don't see a single
// FAILED_DURING_SHUTDOWN signal that ties back to the Stop() that just
// ran. Detect the failure indirectly by comparing lastSavedSeq before vs
// after save(): saveMarshaledSeq Stores(seq) only on the success path
// (R247-GO-15 keeps the gate pinned to the last successful write), so a
// Load() that is still strictly less than the seq we just queued means
// either the staleness gate skipped us (a newer save already raced ahead
// — fine, nothing was lost) or WriteFileAtomic failed (data loss
// imminent at exit). Distinguish by snapshotting before-Load: if a
// newer save did land, before-Load already moved past our pre-save read,
// so the after-Load >= seq case is unambiguous-success. Anything else
// gets the FAILED_DURING_SHUTDOWN tag so log aggregation routes both
// marshal-fail and write-fail to the same alert channel.
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
