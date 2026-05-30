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

// SendPassthrough is the concurrent-capable Send for passthrough mode.
// Unlike Send, it does NOT serialise the entire turn under sendMu — the
// CLI's internal commandQueue plus the Process-level sendSlot FIFO
// provide ordering, and serialising at this layer would defeat
// passthrough's whole point (instant dispatch, tool-boundary mid-turn
// injection).
//
// sendMu is still acquired briefly around the first-turn session-ID
// capture inner-check (see R215-GO-P2-2); the lock window is bounded
// to that critical section and does not span the round-trip.
//
// Callers must verify SupportsPassthrough() before invoking. For protocols
// that don't support replay, the dispatcher should fall back to the legacy
// Send path. Calling SendPassthrough on an unsupported protocol just returns
// an error; it does not hang.
//
// `priority` is one of "", "now", "next", "later". Empty lets the CLI default
// ("next") win. "now" aborts the in-flight turn (see docs/rfc/
// passthrough-mode.md §5.6, validation V2).
func (s *ManagedSession) SendPassthrough(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback, priority string) (*cli.SendResult, error) {
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

	result, err := proc.SendPassthrough(ctx, text, images, onEvent, priority)
	if err != nil {
		s.mapSendError(proc, err)
		return nil, err
	}
	if result.SessionID != "" && s.getSessionID() == "" {
		// Double-check the session-ID capture (R215-GO-P2-2):
		//
		//   1. The outer atomic-Load `s.getSessionID() == ""` is a fast-path
		//      filter — once any prior turn has captured an ID, every later
		//      turn skips the lock entirely (the steady-state cost is one
		//      atomic load).
		//   2. The inner re-check under sendMu enforces correctness when two
		//      concurrent passthrough turns both observe empty on the outer
		//      check: only the first to take sendMu calls onSessionID
		//      (which writes r.sessionIDToKey under r.mu).
		//
		// Without the inner re-check, the second turn could double-invoke
		// onSessionID with a stale-but-equal ID and (in tests) double-count
		// router-side maps. Without the outer check, every passthrough turn
		// would pay sendMu even after the ID is captured.
		//
		// Lock ordering: sendMu → r.mu (top-of-router.go contract). sendMu is
		// only held around the short CAS — it does not serialise the
		// passthrough turn itself, which is the whole point of passthrough.
		s.sendMu.Lock()
		if s.getSessionID() == "" {
			s.setSessionID(result.SessionID)
			if s.onSessionID != nil {
				s.onSessionID(result.SessionID)
			}
		}
		s.sendMu.Unlock()
	}
	return result, nil
}

// SupportsPassthrough exposes the underlying process's passthrough capability
// so the dispatcher can pick between passthrough and legacy Send per session
// (ACP-backed sessions fall back; Claude-backed sessions use passthrough).
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
// deathReason bookkeeping. Shared between Send and SendPassthrough so new
// error sentinels live in one place.
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
func (s *ManagedSession) Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	// Store the cancel func with proc=nil first: the turn is about to run on
	// whatever loadProcess returns below, so a concurrent Interrupt in this
	// window should still fire (it targets the live process). Once proc is
	// known we re-store the box with proc bound, so a later Interrupt fired
	// after a spawnSession swap can detect the mismatch and skip the stale
	// cancel rather than no-op against the new process (SM3 / #381).
	box := &cancelBox{cancel: cancel}
	s.sendCancel.Store(box)
	defer func() {
		s.sendCancel.Store(nil)
		cancel()
	}()

	s.touchLastActive()

	// Cache the user prompt for Snapshot (matches how process.go logs user events).
	prompt := textutil.TruncateRunes(text, 120)
	if len(images) > 0 {
		prompt += " [+" + strconv.Itoa(len(images)) + " image(s)]"
	}
	storeAtomicString(&s.lastPrompt, prompt)

	proc := s.loadProcess()
	if proc == nil {
		return nil, fmt.Errorf("session %s: %w", s.key, ErrNoActiveProcess)
	}
	// Bind the loaded process into the cancel box so Interrupt() can tell
	// whether the cancel func it holds still targets the live process.
	// Re-store (not mutate) because Interrupt reads the box lock-free.
	s.sendCancel.Store(&cancelBox{cancel: cancel, proc: proc})

	// lastActivity tracking is handled lock-free by EventLog.Append via its
	// cached lastActivitySummary; Snapshot() reads that value when the process
	// is alive. Passing onEvent directly (no wrapper closure) avoids a per-Send
	// heap allocation on the nil-callback path (cron/connector) and one less
	// indirect call per event on the Send path.
	result, err := proc.Send(ctx, text, images, onEvent)
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
	return result, nil
}

// Interrupt sends SIGINT to the CLI process and cancels the current Send context.
// This is the equivalent of pressing Escape in Claude Code.
//
// proc.Interrupt() is called BEFORE cancel() to ensure the interrupted flag is
// set before a new Send() can start. proc.Interrupt() only acquires shimWMu
// (not sendMu), so there is no deadlock risk. The subsequent cancel() unblocks
// any in-flight Send() waiting on ctx.Done(), allowing it to release sendMu.
//
// If cancel() were called first, a new Send could race in before proc.Interrupt()
// sets the interrupted flag, causing drainStaleEvents to miss stale events from
// the interrupted turn — the old result would then be returned as the new turn's
// response.
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

// fireSendCancel cancels the in-flight Send()'s context, but only when the
// cancel func still targets the live process (or is not yet bound to any
// process — the box.proc==nil window inside Send before loadProcess). SM3
// (#381): a concurrent spawnSession may have replaced the process pointer
// after Send stored its cancel func; cancelling that stale ctx is a no-op
// against the new live process, so we skip it rather than report a misleading
// success. live is the process Interrupt just observed via loadProcess.
func (s *ManagedSession) fireSendCancel(live processIface) {
	box := s.sendCancel.Load()
	if box == nil || box.cancel == nil {
		return
	}
	// box.proc == nil → Send stored the box but has not bound a process yet;
	// the turn will run on the current live process, so the cancel is valid.
	// box.proc == live → the cancel targets the same process we observed.
	if box.proc == nil || box.proc == live {
		box.cancel()
	}
}

// InterruptOutcome describes what happened on an InterruptViaControl call.
// Callers use this instead of a bare bool so log messages can reflect the
// actual state (e.g. don't claim "aborted turn" when nothing was running).
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
	// InterruptError — transport failure (shim socket dead, write broke);
	// the process-level settle flags have been rolled back. Callers should
	// log this as an error.
	InterruptError
)

// String renders an InterruptOutcome as a stable lowercase tag so slog
// attribute values stay grep-friendly across callers (cron / router /
// dashboard) instead of leaking the iota integer.
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

// InterruptViaControl asks the CLI to abort the active turn by writing an
// in-band control_request to stdin. Unlike Interrupt, this does NOT cancel
// the Send() context — the in-flight Send will see the CLI's interrupted
// result event arrive naturally and return normally, so the owner loop can
// proceed to drain and send the coalesced follow-up messages on the same
// live process.
//
// Transport failures are logged at Warn here (rather than silently returned)
// so operators do not need every caller to plumb their own error log; the
// outcome return value still lets callers tune their user-facing text.
//
// Callers that need to inspect the underlying error (e.g. to errors.Is
// against a specific transport sentinel for triage) should call
// InterruptViaControlDetail instead — see R249-GO-18 (#916).
func (s *ManagedSession) InterruptViaControl() InterruptOutcome {
	outcome, _ := s.InterruptViaControlDetail()
	return outcome
}

// InterruptViaControlDetail mirrors InterruptViaControl but additionally
// returns the underlying error so callers can errors.Is against transport
// sentinels (e.g. distinguish a write-broken socket from a generic protocol
// error). Returned error semantics:
//
//   - InterruptSent       → nil
//   - InterruptNoSession  → nil (no live process to fail against)
//   - InterruptNoTurn     → cli.ErrNoActiveTurn
//   - InterruptUnsupported → cli.ErrInterruptUnsupported
//   - InterruptError      → the wrapped transport error (non-nil)
//
// R249-GO-18 (#916): pre-fix, InterruptError was opaque so cron / dispatch
// could not distinguish "shim socket dead, retry useless" from "stdin
// write returned EAGAIN, retry safe". Adding the err return lets each
// caller errors.Is on specific sentinels without breaking the existing
// outcome-only callers (those keep using InterruptViaControl, which now
// delegates to this method).
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
		// Caller decides whether to fall back; do not escalate to SIGINT
		// silently because that would couple two different semantics.
		return InterruptUnsupported, err
	default:
		// Transport / write error. Process.InterruptViaControl has already
		// rolled back the settle flags, so the next Send() will not spin
		// on the 500ms settle timeout. Surface at Warn so the failure mode
		// is visible even to callers that treat non-Sent as "fall back".
		slog.Warn("session interrupt via control_request failed",
			"key", s.key, "err", err)
		return InterruptError, err
	}
}
