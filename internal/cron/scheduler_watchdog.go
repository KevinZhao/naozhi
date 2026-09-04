// scheduler_watchdog.go: cron send deadline watchdog + error classification —
// the deadline-driven interrupt machinery (abortResult / deadlineInterrupter /
// runDeadlineWatchdog / sendWithWatchdog) plus classifyExecError. A
// self-contained sub-machine the run path calls into; no s.stopCtx reads.

package cron

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// abortResult bundles the watchdog's exit signal: fired reports whether the
// interrupt actually ran (ctx ended via DeadlineExceeded, not success-path
// Cancel — the success path is always fired=false) and outcome is the
// InterruptViaControl result when it did. outcome is the cron-local
// InterruptOutcome (agent_opts.go); internal/wireup/cron_router_adapter.go
// casts session.InterruptOutcome → cron.InterruptOutcome numerically, with
// an init() panic pinning the ordinals.
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

// watchdogInterruptTimeoutDefault caps how long runDeadlineWatchdog waits for
// InterruptViaControl before recording InterruptError and unblocking the
// caller (#507). Unbounded, a wedged control_request channel (stuck stdin
// write) blocks `<-abortCh` forever, finishRun never runs, and
// inflight.running=true makes every later tick skip the job until restart.
// The call itself is not aborted (no underlying ctx); it leaks until session
// teardown, bounded per-run and drained on session.Reset. The effective value
// is per-Scheduler (watchdogInterruptTimeoutNanos, #1904).
const watchdogInterruptTimeoutDefault = 3 * time.Second

// watchdogParkedInterruptGoroutines is a LIVE gauge of inner
// InterruptViaControl goroutines that outlived their watchdog after the
// interrupt-call timeout and are still parked on a wedged stdin write
// (#1632). Unlike the cumulative metrics.CronWatchdogInterruptTimeoutTotal it
// distinguishes "fired N times, all drained" from "N still leaked now": a
// persistent (non-fresh) job that never reaches session.Reset accumulates
// parked goroutines permanently. Incremented when the timeout branch parks
// the goroutine, decremented when it eventually returns.
var watchdogParkedInterruptGoroutines = expvar.NewInt("naozhi_cron_watchdog_parked_interrupt_goroutines")

// abortChanPool recycles the buffer=1 abortResult channels runDeadlineWatchdog
// hands to sendWithWatchdog (otherwise one alloc per tick, #1921). Reuse is
// safe: the channel receives EXACTLY ONE send per run (the AfterFunc callback
// or the nil-guard pre-fill) and sendWithWatchdog either observes stop()==true
// (callback deregistered, channel stays empty) or drains that value before
// returning; the parked InterruptViaControl goroutine writes only to its own
// `done` channel. putAbortChan also drains non-blockingly as a safety net.
var abortChanPool = sync.Pool{New: func() any { return make(chan abortResult, 1) }}

// getAbortChan returns a clean buffer=1 abortResult channel from the pool.
func getAbortChan() chan abortResult { return abortChanPool.Get().(chan abortResult) }

// putAbortChan recycles ch after a non-blocking drain that guarantees it is
// empty for the next user. Only sendWithWatchdog calls this — tests that
// invoke runDeadlineWatchdog directly simply skip the Put (a missed reuse,
// never a correctness bug).
func putAbortChan(ch chan abortResult) {
	select {
	case <-ch:
	default:
	}
	abortChanPool.Put(ch)
}

// watchdogInterruptTimeout reads this Scheduler's active interrupt-call
// timeout. Production callers see watchdogInterruptTimeoutDefault unless a test
// has overridden it on this instance via setWatchdogInterruptTimeoutForScheduler.
func (s *Scheduler) watchdogInterruptTimeout() time.Duration {
	return time.Duration(s.watchdogInterruptTimeoutNanos.Load())
}

// runDeadlineWatchdog arranges for sess.InterruptViaControl to fire exactly
// when ctx ends with DeadlineExceeded. It must run concurrently with sess.Send,
// NOT after: Send's defer flips Process.State Running→Ready the instant ctx
// fires and InterruptViaControl gates on StateRunning (post-Send = no-op).
// context.AfterFunc keeps steady-state watchdog goroutines at ~0 (#492).
//
// The channel is buffer=1, never closed, gets exactly one send, and must be
// treated as receive-only (bidirectional only for pooling, #1921). stop is
// AfterFunc's: true = callback will NOT run, so do not block on the channel;
// false = a value is or will be there (#1705). timeout <= 0 → default (#1904).
func runDeadlineWatchdog(ctx context.Context, sess deadlineInterrupter, timeout time.Duration) (chan abortResult, func() bool) {
	if timeout <= 0 {
		timeout = watchdogInterruptTimeoutDefault
	}
	// Defensive nil guard: a nil ctx panics in context.AfterFunc, a nil sess
	// in the deadline callback — caller bugs, but a panic here would surface
	// as "cron logger" noise without the run recording a result. Return a
	// pre-filled buffer=1 channel so the caller's drain sees a zero
	// abortResult and proceeds with normal finishRun bookkeeping.
	if ctx == nil || sess == nil {
		ch := getAbortChan()
		ch <- abortResult{}
		// No callback registered: a no-op stop reporting false tells the
		// caller "already satisfied, read the channel".
		return ch, func() bool { return false }
	}
	ch := getAbortChan()
	stop := context.AfterFunc(ctx, func() {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			ch <- abortResult{}
			return
		}
		ch <- fireBoundedInterrupt(sess, timeout)
	})
	return ch, stop
}

// fireBoundedInterrupt invokes sess.InterruptViaControl under a timeout and
// returns the abortResult to publish. Shared by runDeadlineWatchdog's
// AfterFunc callback and sendWithWatchdog's synchronous deadline-recovery
// branch (#2021: when stop() wins the race against the callback on an
// already-expired ctx the callback never runs, so the caller fires the
// interrupt itself instead of silently skipping it). InterruptViaControl can
// block indefinitely on a wedged protocol channel (#507); bounding it keeps
// finishRun running and the abortCh slot from outliving Stop's stopBudget.
// done is buffered=1 so the inner goroutine never blocks on send.
func fireBoundedInterrupt(sess deadlineInterrupter, timeout time.Duration) abortResult {
	// timeout <= 0 falls back to the default so a zero-value Scheduler
	// (tests) cannot fire the timeout branch instantly via NewTimer(0).
	if timeout <= 0 {
		timeout = watchdogInterruptTimeoutDefault
	}
	done := make(chan InterruptOutcome, 1)
	// state resolves the leak-gauge race between the inner goroutine and the
	// timeout branch (#1632): 0 = neither acted, 1 = inner returned first
	// (must NOT be counted as parked), 2 = watchdog fired first and parked it
	// (gauge +1; inner decrements on exit). Exactly one CAS(0→1)/CAS(0→2)
	// wins, so the gauge moves at most once per direction per park.
	var state atomic.Int32
	go func() {
		outcome := sess.InterruptViaControl()
		if !state.CompareAndSwap(0, 1) {
			// Watchdog already parked us (state==2) and bumped the gauge;
			// the wedged write finally unblocked — undo the increment.
			watchdogParkedInterruptGoroutines.Add(-1)
		}
		done <- outcome
	}()
	// NewTimer + defer Stop: time.After would leak the timer slot until
	// expiry on the fast path.
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case outcome := <-done:
		return abortResult{outcome: outcome, fired: true}
	case <-t.C:
		// The inner goroutine stays parked on InterruptViaControl until the
		// wedged stdin write unblocks (typically the next session.Reset; for
		// non-fresh jobs maybe never). Surface via counter + Warn (#1327).
		metrics.CronWatchdogInterruptTimeoutTotal.Add(1)
		// CAS(0→2) only wins if the inner goroutine has not yet returned;
		// the matching Add(-1) is in its lost-race branch (#1632).
		if state.CompareAndSwap(0, 2) {
			watchdogParkedInterruptGoroutines.Add(1)
		}
		slog.Warn("cron watchdog: InterruptViaControl timeout exceeded; inner goroutine parked until session reset",
			"timeout", timeout)
		return abortResult{outcome: InterruptError, fired: true}
	}
}

// sendWithWatchdog runs sess.Send under a deadline-watchdog and returns the
// SendResult, the watchdog abortResult, and the Send error. It owns the
// four-step invariant — (1) start watchdog, (2) Send, (3) sendCancel so the
// watchdog returns on the success path, (4) drain abortCh BEFORE the next
// session.Reset so an in-flight interrupt write cannot race the next tick.
//
// Caller contract: sendCtx must be a deadline-bearing ctx (the watchdog fires
// on ctx.Err()==DeadlineExceeded; a Background ctx silently never fires), and
// sendCancel is called here exactly once after Send returns (the caller's
// `defer sendCancel()` is a harmless no-op). Method for the per-instance timeout (#1904).
func (s *Scheduler) sendWithWatchdog(sendCtx context.Context, sendCancel context.CancelFunc, sess Session, text string) (SendResult, abortResult, error) {
	// Watchdog must fire BEFORE Send returns (see runDeadlineWatchdog),
	// otherwise Process.State is already Ready and the interrupt is a no-op.
	abortCh, stopWatchdog := runDeadlineWatchdog(sendCtx, sess, s.watchdogInterruptTimeout())

	// Direct Send without sendWithBroadcast — cron jobs notify via the IM
	// deliverNotice path and the cron_run_ended WS frame.
	result, err := sess.Send(sendCtx, text)

	// Deregister the AfterFunc callback before cancelling ctx (#1705). On the
	// success / non-deadline path the deadline never fired, stopWatchdog()
	// returns true and no callback goroutine is spawned — nothing to drain.
	// false means the callback already fired (deadline, or a cancel racing
	// it) and a value is or will be on abortCh, so cancel + block on it.
	if stopWatchdog() {
		sendCancel()
		// Callback will NOT run, so abortCh is clean — recycle it.
		putAbortChan(abortCh)
		// stop()==true means "callback deregistered", NOT "deadline never
		// fired": if Send returned on the deadline but stop() beat the
		// callback goroutine's start, no interrupt was sent. Fire it
		// synchronously so the CLI turn is still aborted and abort.fired
		// reflects reality (#2021).
		if errors.Is(sendCtx.Err(), context.DeadlineExceeded) {
			return result, fireBoundedInterrupt(sess, s.watchdogInterruptTimeout()), err
		}
		return result, abortResult{}, err
	}

	// Cancel sendCtx so the watchdog returns promptly on the success path
	// (on the deadline path it is already done), then block on abortCh so
	// any InterruptViaControl call completes before the run state is
	// recorded — a fast tick could otherwise overlap the next session.Reset
	// with the in-flight interrupt write.
	sendCancel()
	abort := <-abortCh
	// Drained exactly once; the inner goroutine writes only to its own
	// `done` channel, so no late send can reach the recycled channel.
	putAbortChan(abortCh)
	return result, abort, err
}

// classifyExecError maps a GetOrCreate / Send error to (RunState, ErrorClass)
// for finishRun. defaultClass distinguishes the spawn path
// (ErrClassSessionError) from the send path (ErrClassSendError); the two
// context sentinels are always remapped:
//
//   - context.DeadlineExceeded → (RunStateTimedOut, ErrClassDeadlineExceeded)
//   - context.Canceled         → (RunStateCanceled, ErrClassCanceled)
//
// Order matters: a parent cancel racing DeadlineExceeded can make Send return
// Canceled, so DeadlineExceeded is checked first (deadline WINS on shutdown).
func classifyExecError(err error, defaultClass ErrorClass) (RunState, ErrorClass) {
	if errors.Is(err, context.DeadlineExceeded) {
		return RunStateTimedOut, ErrClassDeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return RunStateCanceled, ErrClassCanceled
	}
	return RunStateFailed, defaultClass
}
