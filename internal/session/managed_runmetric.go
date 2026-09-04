package session

import (
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// runTimer measures one run's wall-clock and first-event latency.
//
// The wrapped onEvent callback may fire on a DIFFERENT goroutine (the CLI
// readLoop) than the one calling finishRun, which on the cancel/bail path can
// read the first-event stamp while a late event is still writing it.
// firstByteNano is therefore an atomic stamped exactly once via CompareAndSwap
// — never a plain field. started is set before the callback is wired and only
// read after the round-trip returns, so it needs no atomic.
type runTimer struct {
	started       time.Time
	firstByteNano atomic.Int64 // unix-nano of first event; 0 = not yet seen
}

// instrumentRun begins timing a run and returns the timer plus an onEvent
// callback to pass to the underlying process. When runStore is nil (tests /
// no-persist) it returns the original callback unwrapped, preserving the
// zero-allocation nil-callback fast path.
func (s *ManagedSession) instrumentRun(onEvent cli.EventCallback) (*runTimer, cli.EventCallback) {
	if s.runStore == nil {
		return nil, onEvent
	}
	rt := &runTimer{started: time.Now()}
	wrapped := func(ev cli.Event) {
		if rt.firstByteNano.Load() == 0 {
			rt.firstByteNano.CompareAndSwap(0, time.Now().UnixNano())
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	return rt, wrapped
}

// finishRun computes the run record from the timer + outcome and enqueues it
// for async persistence. No-op when timing was not instrumented (nil timer /
// nil store). Its work is cheap and the enqueue is NON-BLOCKING, so calling it
// while sendMu is still held (the Send path) does not extend the lock window.
func (s *ManagedSession) finishRun(rt *runTimer, result *cli.SendResult, err error) {
	if rt == nil || s.runStore == nil {
		return
	}
	ended := time.Now()
	oc, cls := runhistory.Classify(err)

	runID, idErr := runhistory.NewRunID()
	if idErr != nil {
		return // crypto/rand unavailable — drop silently, never block the turn
	}

	rec := runhistory.SessionRun{
		RunID:      runID,
		SessionKey: s.key,
		SessionID:  s.getSessionID(),
		StartedAt:  rt.started,
		EndedAt:    ended,
		DurationMS: ended.Sub(rt.started).Milliseconds(),
		Outcome:    oc,
		ErrorClass: cls,
	}
	// Atomic load: a late readLoop event may still be CAS-stamping it.
	if fb := rt.firstByteNano.Load(); fb != 0 {
		rec.FirstByteMS = time.Unix(0, fb).Sub(rt.started).Milliseconds()
		if rec.FirstByteMS < 0 {
			rec.FirstByteMS = 0
		}
	}
	if result != nil {
		// result.CostUSD is the CLI's cumulative cost for the current process
		// incarnation (resets on resume/restart); convert to a per-turn delta and
		// fold into the monotonic total. costMu serialises the read-compute-store:
		// under passthrough concurrent same-session turns call finishRun on
		// separate goroutines and would otherwise interleave and lose an update.
		s.costMu.Lock()
		prev := loadTotalCost(&s.lastCumulativeCost)
		delta, next := runhistory.TurnCostDelta(result.CostUSD, prev)
		storeTotalCost(&s.lastCumulativeCost, next)
		if delta != 0 {
			storeTotalCost(&s.costSpent, loadTotalCost(&s.costSpent)+delta)
		}
		s.costMu.Unlock()
		rec.CostUSD = delta
	}
	s.runStore.AppendAsync(rec)
}
