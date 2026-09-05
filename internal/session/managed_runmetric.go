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

// finishRun accounts the turn's cost (always) and, when timing was
// instrumented (non-nil timer / store), enqueues the run record for async
// persistence. Its work is cheap and the enqueue is NON-BLOCKING, so calling
// it while sendMu is still held (the Send path) does not extend the lock window.
func (s *ManagedSession) finishRun(rt *runTimer, proc processIface, result *cli.SendResult, err error) {
	runID := newRunID()
	delta := s.accountTurnCost(result, runID)
	if result == nil && err != nil {
		// proc is the process that ran the turn: passthrough holds no sendMu,
		// so loadProcess() here could already be a replacement.
		s.bookPartialTurn(proc, err, runID)
	}
	if rt == nil || s.runStore == nil || runID == "" {
		return
	}
	ended := time.Now()
	oc, cls := runhistory.Classify(err)

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
	rec.CostUSD = delta
	s.runStore.AppendAsync(rec)
}
