package session

import (
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// runTimer measures one run's wall-clock and first-event latency.
//
// The wrapped onEvent callback may fire on a DIFFERENT goroutine than the one
// that calls finishRun: on the passthrough path onEvent runs on the CLI
// readLoop goroutine, and on the cancel/bail return path finishRun can read
// the first-event stamp while a late readLoop event is still writing it.
// firstByteNano is therefore an atomic stamped exactly once via CompareAndSwap
// — never a plain field — so the cross-goroutine read in finishRun is
// race-free regardless of send path. started is set once before the callback
// is wired and only read after the round-trip returns, so it needs no atomic.
type runTimer struct {
	started       time.Time
	firstByteNano atomic.Int64 // unix-nano of first event; 0 = not yet seen
}

// instrumentRun begins timing a run and returns the timer plus an onEvent
// callback to pass to the underlying process. When runStore is nil (tests /
// no-persist) it returns the original callback unwrapped, preserving the
// existing zero-allocation nil-callback fast path. The wrapper records the
// first event's timestamp exactly once (CAS 0 -> now), tolerating concurrent
// fan-out from the readLoop goroutine on the passthrough path.
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
// nil store). The only work it does is cheap (time.Now, Classify, an 8-byte
// crypto/rand read) followed by a NON-BLOCKING channel enqueue, so calling it
// while sendMu is still held (the Send path) does not extend the lock window
// in any observable way; the SendPassthrough path is already lock-free.
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
	// Atomic load: race-free even if a late readLoop event is concurrently
	// CAS-stamping it on the passthrough cancel/bail path.
	if fb := rt.firstByteNano.Load(); fb != 0 {
		rec.FirstByteMS = time.Unix(0, fb).Sub(rt.started).Milliseconds()
		if rec.FirstByteMS < 0 {
			rec.FirstByteMS = 0
		}
	}
	if result != nil {
		rec.CostUSD = result.CostUSD
	}
	s.runStore.AppendAsync(rec)
}
