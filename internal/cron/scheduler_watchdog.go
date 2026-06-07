// scheduler_watchdog.go: cron send deadline watchdog + error classification.
//
// Split out of scheduler_run.go (move-only, no behaviour change): the
// deadline-driven interrupt machinery (abortResult / deadlineInterrupter /
// runDeadlineWatchdog / sendWithWatchdog) plus classifyExecError. These have
// no s.stopCtx code-reads and no finishRun transaction concerns — they are a
// self-contained sub-machine the run path calls into. Methods/functions stay
// in package cron so private fields remain accessible without exporting.

package cron

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// abortResult bundles the watchdog's exit signal: whether it actually
// fired the interrupt (i.e. the ctx ended via DeadlineExceeded, not via
// success-path Cancel) and what the InterruptViaControl outcome was when
// it did. The fired flag is the discriminator the caller logs.
//
// outcome is the cron-local InterruptOutcome; the production adapter
// in internal/wireup/cron_router_adapter.go casts session.InterruptOutcome
// → cron.InterruptOutcome via a numeric cast, with an init() panic
// pinning the ordinals.
//
// R238-ARCH-20 (#787) proposed renaming deadlineInterrupter →
// RunInterrupter and switching abortResult to a fresh InterruptResult
// enum to "break the dependency on session.InterruptOutcome". The
// decoupling is already complete: the cron-local InterruptOutcome above
// (defined in agent_opts.go) is the public type; cron does NOT import
// session.InterruptOutcome anywhere in production code (the last
// reverse import was eliminated by R20260527122801-ARCH-1, see the
// banner in scheduler_session.go). The proposed rename is a cosmetic
// preference rather than an architectural fix; deferring keeps the
// type name aligned with the concept "deadline-driven interrupt"
// across godoc / metrics / tests, and avoids a sweep across N test
// files. The fired-vs-success ambiguity flagged in the issue is
// addressed by abortResult.fired's godoc above (success path is
// fired=false; only the watchdog firing sets fired=true).
type abortResult struct {
	outcome InterruptOutcome
	fired   bool
}

// deadlineInterrupter is the narrow capability runDeadlineWatchdog needs
// from a session: a way to abort an in-flight CLI turn via the protocol's
// control_request channel. cron.Session satisfies this; cron tests
// stub it with a counting mock to assert the watchdog fired exactly when
// the deadline elapsed without having to also implement Send.
type deadlineInterrupter interface {
	InterruptViaControl() InterruptOutcome
}

// watchdogInterruptTimeoutDefault caps how long runDeadlineWatchdog
// will wait for InterruptViaControl to return before recording the
// attempt as InterruptError and unblocking the caller. R236-GO-09 (#507):
// pre-fix, a wedged session.InterruptViaControl (control_request channel
// pinned by a stuck stdin write or a kernel-blocked syscall) would hold
// the goroutine forever; the caller's `<-abortCh` then blocked forever
// and finishRun was never invoked, leaving inflight.running=true so
// every subsequent tick skipped the job until process restart. Bounding
// the call at 3s lets finishRun fire on the recovery path so the next
// tick has a chance to spawn a fresh session. The InterruptViaControl
// call itself is not aborted (no underlying ctx) — it leaks until the
// session teardown unblocks it, but the leak is bounded (per-run,
// drained on session.Reset) and far less harmful than a permanently
// stuck job.
const watchdogInterruptTimeoutDefault = 3 * time.Second

// watchdogInterruptTimeoutAtomic stores the effective timeout in
// nanoseconds. Atomic so the timeout regression tests can shorten it
// (typically to 50ms so they don't burn 3s of CI wall time) without
// racing the production read in the watchdog goroutine. Tests must
// always restore the previous value (use setWatchdogInterruptTimeoutForTest,
// which registers the restore on t.Cleanup) and must NOT call t.Parallel().
//
// R20260607-GO-4 (#1904): this is package-level mutable state, so two
// t.Parallel() tests that each shorten it would clobber each other — one
// test's 50ms override could bleed into another's watchdog read. The clean
// fix mirrors Scheduler.stopBudget (R249-CR-3 / #947): move the timeout onto
// a per-Scheduler field so each instance is isolated. That is deferred here
// because runDeadlineWatchdog / sendWithWatchdog are *package-level
// functions* (no *Scheduler receiver) called from scheduler_run.go; threading
// a per-instance timeout would require changing the Scheduler struct
// definition (scheduler.go) and the call site (scheduler_run.go), both
// outside this file's change scope. As a contained mitigation the override
// is funnelled through setWatchdogInterruptTimeoutForTest below so the
// snapshot+restore discipline is enforced in one place rather than copied
// (and occasionally fumbled) across every timeout test, and the no-Parallel
// constraint is documented at the single seam. When the per-Scheduler field
// lands, delete this var, the helper, and the constraint.
var watchdogInterruptTimeoutAtomic atomic.Int64

// watchdogParkedInterruptGoroutines is a LIVE gauge of inner
// InterruptViaControl goroutines that outlived their watchdog after the
// interrupt-call timeout fired and are still parked on a wedged stdin
// write (R20260602-GO-005, #1632). It differs from
// metrics.CronWatchdogInterruptTimeoutTotal, which only counts cumulative
// timeout events: a persistent (non-fresh) cron job that never reaches
// session.Reset can accumulate permanently-parked goroutines, and the
// cumulative counter cannot distinguish "fired N times, all since
// drained" from "N still leaked right now". This gauge is incremented
// when the timeout branch parks the inner goroutine and decremented when
// that goroutine eventually returns (if ever), so operators can alert on
// a steadily rising live value rather than inferring it from process
// goroutine growth. expvar registration is package-global; the var stays
// in cron's file domain (no internal/metrics edit) since it observes a
// cron-internal lifecycle.
var watchdogParkedInterruptGoroutines = expvar.NewInt("naozhi_cron_watchdog_parked_interrupt_goroutines")

func init() {
	watchdogInterruptTimeoutAtomic.Store(int64(watchdogInterruptTimeoutDefault))
}

// watchdogInterruptTimeout reads the active interrupt-call timeout.
// Production callers see watchdogInterruptTimeoutDefault unless a test
// has overridden it via the atomic.
func watchdogInterruptTimeout() time.Duration {
	return time.Duration(watchdogInterruptTimeoutAtomic.Load())
}

// runDeadlineWatchdog arranges for sess.InterruptViaControl to fire
// exactly when ctx ends with DeadlineExceeded. The interrupt must run
// concurrently with sess.Send, NOT after — Send's internal defer flips
// Process.State Running→Ready the instant ctx fires, and
// InterruptViaControl gates on State==StateRunning, so calling it
// post-Send is dead code (returns ErrNoActiveTurn → outcome=no_turn).
//
// Channel contract (R249-CR-27): the returned channel has buffer=1 and
// is intentionally NOT closed. The publishing goroutine self-completes
// thanks to buffer=1 — its single send never blocks, so it returns
// regardless of whether the caller reads. The caller drains ch only to
// observe the abort outcome (abort.fired / abort.outcome) for logging
// and to ensure InterruptViaControl has finished before recording the
// run state; failing to drain leaks the abortResult value, NOT the
// goroutine, and is harmless for shutdown bookkeeping.
//
// On the success / non-deadline error path the caller cancels ctx
// explicitly; the publishing callback observes ctx.Err()==Canceled,
// skips InterruptViaControl, and returns abortResult{fired:false}.
//
// R247-GO-12 (#492): we use context.AfterFunc rather than spawning a
// long-lived `<-ctx.Done()` goroutine. With per-tick CAS-protected runs,
// a 50-job @ 1Hz deployment otherwise holds ~50 watchdog goroutines
// concurrently for the duration of every Send (up to jobTimeout). With
// AfterFunc the runtime only spawns a goroutine when ctx ends — briefly,
// to invoke the callback — so the steady-state in-flight watchdog
// goroutine count drops from O(in-flight runs) to ~0. The deadline /
// cancel semantics are preserved exactly: the callback inspects
// ctx.Err() the same way the goroutine used to.
//
// R20260603140013-GO-1 (#1705): returns context.AfterFunc's stop fn so
// the caller can deregister the callback on the success / non-deadline
// path BEFORE cancelling ctx. Without it, sendCancel() always ends ctx
// with Canceled, the runtime still spawns the callback goroutine, and it
// does a wasted chan send of a zero abortResult{} — one extra goroutine
// spawn + chan send per successful cron Send (and 500 at once on a
// 500-job shutdown burst). When stop() returns true the callback will
// NOT run, so the caller MUST NOT block on the channel; stop() returning
// false means the callback already fired (deadline path or a cancel that
// raced the deadline) and a value is or will be on the channel. The nil-
// guard fast path returns a no-op stop (callback already satisfied).
func runDeadlineWatchdog(ctx context.Context, sess deadlineInterrupter) (<-chan abortResult, func() bool) {
	// R249-GO-3: defensive nil guard. A nil ctx would panic on
	// context.AfterFunc; a nil sess would panic on InterruptViaControl
	// when the deadline path fires. Both are caller bugs (production wires
	// real values), but the cron run goroutine swallows panics via
	// robfig/cron's recover chain elsewhere — here a panic would surface as
	// "cron logger" Error noise without the run ever recording a result.
	// Return a pre-completed channel so the caller's `<-abortCh` sees a
	// zero abortResult and proceeds with normal finishRun bookkeeping.
	// Buffer=1 with no close mirrors the success-path contract: the caller
	// drains exactly once; an unclosed channel of buffer=1 with one send
	// already buffered satisfies that without leaking a goroutine.
	if ctx == nil || sess == nil {
		ch := make(chan abortResult, 1)
		ch <- abortResult{}
		// No callback was registered, so the result is already on ch:
		// return a no-op stop reporting false (== "already satisfied, read
		// the channel") so the caller's success-path drain still works.
		return ch, func() bool { return false }
	}
	ch := make(chan abortResult, 1)
	stop := context.AfterFunc(ctx, func() {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			ch <- abortResult{}
			return
		}
		// R236-GO-09 (#507) / R247-GO-5 (#476) / R246-GO-6: InterruptViaControl
		// can block indefinitely when the protocol channel is wedged
		// (kernel-blocked stdin write, control_request never acked). Bound
		// it so the caller always observes a result on abortCh and
		// finishRun runs — otherwise inflight.running stays true and every
		// subsequent tick silently skips the job AND the wrapper goroutine
		// holds the abortCh slot past Stop's stopBudget during scheduler
		// shutdown (the systemd TimeoutStopSec failure mode the R247-GO-5
		// anchor explicitly cited). The done channel is buffered=1 so the
		// inner goroutine never blocks on send: it returns whenever
		// InterruptViaControl finishes, even after the timeout branch has
		// already published an InterruptError outcome.
		done := make(chan InterruptOutcome, 1)
		// state coordinates the live leak-gauge accounting between this
		// inner goroutine and the timeout branch below (R20260602-GO-005,
		// #1632). It is a 3-state CAS race resolver:
		//   0 = neither side has acted yet
		//   1 = inner goroutine returned first (watchdog must NOT park it)
		//   2 = watchdog fired first and parked the inner goroutine
		//       (gauge incremented; inner goroutine must decrement on exit)
		// Exactly one of the two CAS(0→1)/CAS(0→2) wins, so the gauge is
		// incremented and later decremented at most once per park — no
		// leak under any interleaving of "inner returns" vs "watchdog
		// fires".
		var state atomic.Int32
		go func() {
			outcome := sess.InterruptViaControl()
			if !state.CompareAndSwap(0, 1) {
				// Lost the race: watchdog already parked us (state==2) and
				// incremented the gauge. We outlived the watchdog but the
				// wedged write finally unblocked — undo the increment.
				watchdogParkedInterruptGoroutines.Add(-1)
			}
			done <- outcome
		}()
		// R20260527122801-GO-001: NewTimer + defer Stop mirrors
		// scheduler.go:1337 — time.After leaks a *Timer slot until
		// expiry on the success path.
		t := time.NewTimer(watchdogInterruptTimeout())
		defer t.Stop()
		select {
		case outcome := <-done:
			ch <- abortResult{outcome: outcome, fired: true}
		case <-t.C:
			// R20260527122801-SEC-3 (#1327): the inner goroutine above
			// is still parked on InterruptViaControl and will outlive
			// this watchdog goroutine until the wedged stdin write
			// unblocks (typically on the next session.Reset; for non-
			// fresh jobs that may never happen on its own). Surface the
			// event via a metric + Warn log so operators can alert on
			// rising deltas rather than discovering it via slow goroutine
			// growth. The metric lives next to other cron counters in
			// internal/metrics so the dashboard wireup is identical.
			metrics.CronWatchdogInterruptTimeoutTotal.Add(1)
			// R20260602-GO-005 (#1632): record the parked goroutine on a
			// LIVE gauge so a persistent (never-reset) job's permanent
			// leak is observable as a rising current count, not just a
			// cumulative timeout total. CAS(0→2) only wins if the inner
			// goroutine has not already returned; if it lost the race the
			// goroutine is gone and there is nothing to count. The matching
			// Add(-1) lives in the inner goroutine's lost-race branch.
			if state.CompareAndSwap(0, 2) {
				watchdogParkedInterruptGoroutines.Add(1)
			}
			slog.Warn("cron watchdog: InterruptViaControl timeout exceeded; inner goroutine parked until session reset",
				"timeout", watchdogInterruptTimeout())
			ch <- abortResult{outcome: InterruptError, fired: true}
		}
	})
	return ch, stop
}

// sendWithWatchdog runs sess.Send under a deadline-watchdog and returns
// the SendResult, the watchdog abortResult, and the Send error in one
// shot. R215-ARCH-P2-5 (#581) partial: factored out of executeOpt so
// the four-step invariant — (1) start watchdog, (2) Send, (3)
// sendCancel so the watchdog returns on the success path, (4) drain
// abortCh BEFORE the next session.Reset to avoid the in-flight
// interrupt write racing the next tick — lives in one named function
// instead of inlined in a 569-line state machine where a future split
// could accidentally reorder the cancel/drain pair.
//
// Caller contract:
//   - sendCtx must be a context.WithTimeout / s.stopCtx-derived ctx.
//     Watchdog uses ctx.Err() == DeadlineExceeded as its fire trigger;
//     Background or any non-deadline ctx degrades to "interrupt never
//     fires" silently.
//   - sendCancel is called by this helper exactly once after Send
//     returns; the caller's `defer sendCancel()` is therefore a no-op
//     (cancelFunc is idempotent).
func sendWithWatchdog(sendCtx context.Context, sendCancel context.CancelFunc, sess Session, text string) (SendResult, abortResult, error) {
	// Watchdog: deadline-fired interrupt of the in-flight CLI turn. See
	// runDeadlineWatchdog for the rationale (must fire BEFORE Send
	// returns, otherwise Process.State has already flipped to Ready and
	// InterruptViaControl returns ErrNoActiveTurn → no-op).
	abortCh, stopWatchdog := runDeadlineWatchdog(sendCtx, sess)

	// Direct Send without sendWithBroadcast — cron jobs notify via the
	// IM deliverNotice path (resolveNotifyTarget + platform.Reply) and
	// the cron_run_ended WS frame.
	result, err := sess.Send(sendCtx, text)

	// R20260603140013-GO-1 (#1705): deregister the AfterFunc callback
	// before cancelling ctx. On the success / non-deadline-error path the
	// deadline never fired, so stopWatchdog() returns true and the runtime
	// never spawns the callback goroutine — we save a goroutine spawn +
	// chan send per Send (×500 on a shutdown burst). When stop succeeds the
	// callback will NOT run, so there is nothing to drain: synthesize the
	// not-fired result the callback would have published. When it returns
	// false the callback already fired (deadline path, or a cancel that
	// raced the deadline) and a value is or will be on abortCh, so we still
	// cancel + block on it to keep the original ordering guarantee (the
	// in-flight InterruptViaControl must finish before the next
	// session.Reset).
	if stopWatchdog() {
		sendCancel()
		return result, abortResult{}, err
	}

	// Cancel sendCtx so the watchdog returns promptly on the success /
	// non-deadline error path; on the deadline path it's already done.
	// Block on abortCh so the InterruptViaControl call (if any)
	// completes before we record the run state — otherwise a fast cron
	// tick could overlap the next session.Reset with the in-flight
	// interrupt write.
	sendCancel()
	abort := <-abortCh
	return result, abort, err
}

// classifyExecError maps an error from GetOrCreate or Send to
// (RunState, ErrorClass) for finishRun. defaultClass distinguishes the
// session-spawn path (ErrClassSessionError) from the send path
// (ErrClassSendError); the helper unconditionally remaps the two
// context-derived sentinels:
//
//   - context.DeadlineExceeded → (RunStateTimedOut, ErrClassDeadlineExceeded)
//   - context.Canceled         → (RunStateCanceled, ErrClassCanceled)
//
// R241-ARCH-7: Canceled was historically handled by the caller via a
// dedicated `if errors.Is(err, context.Canceled)` branch ahead of this
// helper, so the state mapping was split across this site (DeadlineExceeded
// only) and the two caller blocks (Canceled / default). Folding Canceled
// into the helper keeps all (err → state, errClass) decisions in one
// place. Callers still own the side-effects that DIFFER per class
// (skipPersist=true for Canceled, operator-facing notice suppressed for
// Canceled, abort.fired logging on the send path) — see executeOpt's
// switch on errClass below for those policy choices.
//
// errors.Is order matters: context.Canceled wraps both genuine
// cancellation AND the "parent ctx cancelled mid-DeadlineExceeded" race
// where Send returns context.Canceled even though the deadline ticked
// first. Checking DeadlineExceeded first preserves the historical
// classification (deadline-exceeded WINS) so jobs that hit jobTimeout
// during a graceful shutdown still record RunStateTimedOut rather than
// RunStateCanceled. R230C-CR-7 (original) + R241-ARCH-7 (Canceled fold).
func classifyExecError(err error, defaultClass ErrorClass) (RunState, ErrorClass) {
	if errors.Is(err, context.DeadlineExceeded) {
		return RunStateTimedOut, ErrClassDeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return RunStateCanceled, ErrClassCanceled
	}
	return RunStateFailed, defaultClass
}
