package session

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
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
func (s *ManagedSession) instrumentRun(onEvent clievent.EventCallback) (*runTimer, clievent.EventCallback) {
	if s.runStore == nil {
		return nil, onEvent
	}
	rt := &runTimer{started: time.Now()}
	wrapped := func(ev clievent.Event) {
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
func (s *ManagedSession) finishRun(ctx context.Context, rt *runTimer, result *clievent.SendResult, err error) {
	// The orchestrator's run id (ctxutil.WithRunID) names this turn in the
	// logs; adopting it keeps the run record and the journal on one key.
	runID := ctxutil.RunID(ctx)
	if runID == "" {
		runID = newRunID()
	}
	delta := s.accountTurnCost(result, runID)
	if rt == nil || s.runStore == nil || runID == "" {
		return
	}
	ended := time.Now()
	oc, cls := runhistory.Classify(err)
	// A new session captures its ID only after this returns, so its first
	// run would otherwise be recorded without the CLI session it ran in.
	sid := s.getSessionID()
	if sid == "" && result != nil {
		sid = result.SessionID
	}

	rec := runhistory.SessionRun{
		RunID:      runID,
		SessionKey: s.key,
		SessionID:  sid,
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

// nudgeRunCtx gives the leaked-toolcall re-send its own run id: it is a
// second run record and ledger row, and both are keyed by the id. The log
// line ties it to the turn it continues.
func nudgeRunCtx(ctx context.Context) context.Context {
	parent := ctxutil.RunID(ctx)
	ctx = ctxutil.WithRunID(ctx, newRunID())
	slog.InfoContext(ctx, "leak-recovery: nudge run", "nudge_of", parent)
	return ctx
}
