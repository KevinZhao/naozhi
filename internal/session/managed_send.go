package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/textutil"
)

// SendPassthrough is the concurrent-capable Send for passthrough mode. Unlike
// Send it does NOT serialise the turn under sendMu — the CLI's commandQueue
// plus the Process-level sendSlot FIFO provide ordering. sendMu is acquired
// only briefly around the first-turn session-ID capture.
//
// Callers must verify SupportsPassthrough() first; on an unsupported protocol
// this returns an error rather than hanging.
//
// `priority` is one of "", "now", "next", "later"; empty lets the CLI default
// ("next") win. "now" aborts the in-flight turn (docs/rfc/passthrough-mode.md §5.6).
func (s *ManagedSession) SendPassthrough(ctx context.Context, text string, images []cli.Attachment, onEvent cli.EventCallback, priority string) (*cli.SendResult, error) {
	s.touchLastActive()

	prompt := textutil.TruncateRunes(text, 120)
	if len(images) > 0 {
		prompt += " [+" + strconv.Itoa(len(images)) + " image(s)]"
	}
	storeAtomicString(&s.lastPrompt, prompt)

	proc := s.loadProcess()
	if proc == nil {
		return nil, fmt.Errorf("session %s: %w", s.key, ErrNoActiveProcess)
	}

	rt, evCb := s.instrumentRun(onEvent)
	result, err := proc.SendPassthrough(ctx, text, images, evCb, priority)
	s.finishRun(rt, proc, result, err)
	if err != nil {
		s.mapSendError(proc, err)
		return nil, err
	}
	if result.SessionID != "" && s.getSessionID() == "" {
		// Double-checked session-ID capture: the outer atomic load skips the
		// lock once any turn has captured an ID; the inner re-check under
		// sendMu ensures only the first of two concurrent turns calls
		// onSessionID (which writes r.ss.idToKey under r.mu). Lock ordering:
		// sendMu → r.mu; sendMu is held only around this short CAS.
		s.sendMu.Lock()
		if s.getSessionID() == "" {
			s.setSessionID(result.SessionID)
			if s.onSessionID != nil {
				s.onSessionID(result.SessionID)
			}
		}
		s.sendMu.Unlock()
	}

	// Auto-continue a stalled leaked-tool-call turn. sendMu is NOT held across
	// the round-trip, so the nudge uses priority="next" to enqueue right after
	// the leaked turn — a racing user message cannot jump the FIFO. Recovery
	// completes before this method returns, strictly upstream of any channel
	// flush, so feishu/weixin never see the leaked XML.
	result = s.recoverLeakedToolcall(ctx, proc, result, func(rctx context.Context, nudge string) (*cli.SendResult, error) {
		rrt, revCb := s.instrumentRun(onEvent)
		rr, rerr := proc.SendPassthrough(rctx, nudge, nil, revCb, "next")
		s.finishRun(rrt, proc, rr, rerr)
		return rr, rerr
	})
	return result, nil
}

// SupportsPassthrough exposes the underlying process's passthrough capability
// so the dispatcher can pick between passthrough and legacy Send per session.
func (s *ManagedSession) SupportsPassthrough() bool {
	proc := s.loadProcess()
	if proc == nil {
		return false
	}
	return proc.SupportsPassthrough()
}

// DiscardPassthroughPending delegates to the process's pending-slot cleanup.
// Called on /new, /clear, and forced session reset.
func (s *ManagedSession) DiscardPassthroughPending(reason error) {
	proc := s.loadProcess()
	if proc == nil {
		return
	}
	proc.DiscardPassthroughPending(reason)
}

// PassthroughDepth is a read-only view of pending slots for dashboard /
// status display.
func (s *ManagedSession) PassthroughDepth() int {
	proc := s.loadProcess()
	if proc == nil {
		return 0
	}
	return proc.PassthroughDepth()
}

// mapSendError translates Process-level errors into ManagedSession
// deathReason bookkeeping. Shared between Send and SendPassthrough.
func (s *ManagedSession) mapSendError(proc processIface, err error) {
	switch {
	case errors.Is(err, cli.ErrNoOutputTimeout):
		storeAtomicString(&s.deathReason, "no_output_timeout")
	case errors.Is(err, cli.ErrTotalTimeout):
		storeAtomicString(&s.deathReason, "total_timeout")
	case errors.Is(err, cli.ErrProcessExited):
		reason := "process_exited"
		if dr := proc.DeathReason(); dr != "" {
			reason = dr
		}
		storeAtomicString(&s.deathReason, reason)
	}
}

// Send delivers a message to the claude process and returns the result.
// Messages to the same session are serialized via sendMu.
func (s *ManagedSession) Send(ctx context.Context, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	// Store the cancel func with proc=nil first so a concurrent Interrupt in
	// this window still fires; once proc is known the box is re-stored with
	// proc bound, letting a later Interrupt detect a spawnSession swap and skip
	// the stale cancel (#381).
	box := &cancelBox{cancel: cancel}
	s.sendCancel.Store(box)
	defer func() {
		s.sendCancel.Store(nil)
		cancel()
	}()

	s.touchLastActive()

	// Cache the user prompt for Snapshot.
	prompt := textutil.TruncateRunes(text, 120)
	if len(images) > 0 {
		prompt += " [+" + strconv.Itoa(len(images)) + " image(s)]"
	}
	storeAtomicString(&s.lastPrompt, prompt)

	proc := s.loadProcess()
	if proc == nil {
		return nil, fmt.Errorf("session %s: %w", s.key, ErrNoActiveProcess)
	}
	// Re-store (not mutate) the box with proc bound: Interrupt reads it lock-free.
	s.sendCancel.Store(&cancelBox{cancel: cancel, proc: proc})

	// lastActivity is tracked lock-free by EventLog.Append. onEvent is passed
	// without a wrapper closure to avoid a per-Send allocation on the
	// nil-callback path; instrumentRun only wraps when runStore is set.
	rt, evCb := s.instrumentRun(onEvent)
	result, err := proc.Send(ctx, text, images, evCb)
	s.finishRun(rt, proc, result, err)
	if err != nil {
		s.mapSendError(proc, err)
		return nil, err
	}

	// Capture session ID from first successful send
	if s.getSessionID() == "" && result.SessionID != "" {
		s.setSessionID(result.SessionID)
		if s.onSessionID != nil {
			s.onSessionID(result.SessionID)
		}
	}

	// Auto-continue a turn stalled by a tool call leaked into prose (no-op
	// unless enabled and the text actually leaks). Runs while sendMu is held,
	// so the re-send is serial with any other turn; its <system-reminder>
	// nudge lands in EventLog but is hidden by dashboard.js's filter.
	result = s.recoverLeakedToolcall(ctx, proc, result, func(rctx context.Context, nudge string) (*cli.SendResult, error) {
		rrt, revCb := s.instrumentRun(onEvent)
		rr, rerr := proc.Send(rctx, nudge, nil, revCb)
		s.finishRun(rrt, proc, rr, rerr)
		return rr, rerr
	})
	return result, nil
}

// Interrupt sends SIGINT to the CLI process and cancels the current Send
// context (the equivalent of pressing Escape in Claude Code).
//
// proc.Interrupt() runs BEFORE cancel() so the interrupted flag is set before
// a new Send() can start; otherwise drainStaleEvents could miss stale events
// and return the old result as the new turn's response. proc.Interrupt() only
// takes shimWMu (not sendMu), so there is no deadlock risk; cancel() then
// unblocks any in-flight Send() waiting on ctx.Done() so it releases sendMu.
func (s *ManagedSession) Interrupt() bool {
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		// Still cancel in case Send is blocked on ctx.Done().
		s.fireSendCancel(proc)
		return false
	}

	proc.Interrupt()

	s.fireSendCancel(proc)
	return true
}

// fireSendCancel cancels the in-flight Send()'s context only when the cancel
// func still targets the live process, or is not yet bound to any process
// (box.proc==nil window inside Send before loadProcess). A concurrent
// spawnSession may have replaced the process after Send stored its cancel;
// cancelling that stale ctx would be a no-op against the new process, so it is
// skipped rather than reported as success (#381).
//
// live==nil means Interrupt observed no live process, so there is no
// stale-binding risk and a Send still blocked on ctx.Done() must be unblocked
// regardless of which now-dead process its box was bound to.
func (s *ManagedSession) fireSendCancel(live processIface) {
	box := s.sendCancel.Load()
	if box == nil || box.cancel == nil {
		return
	}
	if box.proc == nil || box.proc == live || live == nil {
		box.cancel()
	}
}

// InterruptOutcome describes what happened on an InterruptViaControl call, so
// log messages can reflect the actual state instead of a bare bool.
type InterruptOutcome int

const (
	// InterruptSent — a control_request reached the CLI; the active turn
	// will produce a final result shortly and the next Send() will drain it.
	InterruptSent InterruptOutcome = iota
	// InterruptNoSession — session does not exist or has no live process.
	InterruptNoSession
	// InterruptNoTurn — session is alive but idle; nothing was interrupted.
	InterruptNoTurn
	// InterruptUnsupported — protocol does not support stdin-level interrupt
	// (e.g. ACP). Callers may fall back to Interrupt() for SIGINT semantics.
	InterruptUnsupported
	// InterruptError — transport failure; the process-level settle flags have
	// been rolled back. Callers should log this as an error.
	InterruptError
)

// String renders an InterruptOutcome as a stable lowercase tag for slog attrs.
func (o InterruptOutcome) String() string {
	switch o {
	case InterruptSent:
		return "sent"
	case InterruptNoSession:
		return "no_session"
	case InterruptNoTurn:
		return "no_turn"
	case InterruptUnsupported:
		return "unsupported"
	case InterruptError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}

// InterruptViaControl asks the CLI to abort the active turn via an in-band
// control_request on stdin. Unlike Interrupt, it does NOT cancel the Send()
// context — the in-flight Send sees the CLI's interrupted result arrive
// naturally, so the owner loop can drain and send coalesced follow-ups on the
// same live process. Transport failures are logged at Warn here so callers
// need not plumb their own error log; use InterruptViaControlDetail to inspect
// the underlying error (#916).
func (s *ManagedSession) InterruptViaControl() InterruptOutcome {
	outcome, _ := s.InterruptViaControlDetail()
	return outcome
}

// InterruptViaControlDetail mirrors InterruptViaControl but also returns the
// underlying error so callers can errors.Is against transport sentinels (#916):
//
//   - InterruptSent       → nil
//   - InterruptNoSession  → nil (no live process to fail against)
//   - InterruptNoTurn     → cli.ErrNoActiveTurn
//   - InterruptUnsupported → cli.ErrInterruptUnsupported
//   - InterruptError      → the wrapped transport error (non-nil)
func (s *ManagedSession) InterruptViaControlDetail() (InterruptOutcome, error) {
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		return InterruptNoSession, nil
	}
	err := proc.InterruptViaControl()
	if err == nil {
		return InterruptSent, nil
	}
	switch {
	case errors.Is(err, cli.ErrNoActiveTurn):
		return InterruptNoTurn, err
	case errors.Is(err, cli.ErrInterruptUnsupported):
		// Caller decides whether to fall back; escalating to SIGINT silently
		// would couple two different semantics.
		return InterruptUnsupported, err
	default:
		// Process.InterruptViaControl has already rolled back the settle
		// flags, so the next Send() will not spin on the settle timeout.
		slog.Warn("session interrupt via control_request failed",
			"key", s.key, "err", err)
		return InterruptError, err
	}
}
