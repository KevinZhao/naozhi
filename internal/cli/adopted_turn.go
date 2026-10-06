package cli

// adopted_turn.go — the result of a turn that was already in flight when this
// process reconnected to a surviving shim (docs/rfc/cron-run-adoption.md §3).
//
// Why a latch and not a waiter: nothing else keeps that result. The Send that
// started the turn ran in a PREVIOUS naozhi process, so no in-process caller
// owns it — deliverEvent's handoff into eventCh is non-blocking and nobody is
// draining it, and the next Send's drainStaleEvents discards what is left. The
// only durable copy, ring.EventLog, records a result as turn-boundary metadata
// with no text at all (see process_event_format.go's "result" case). So the
// answer has to be captured where it arrives, not fetched where it is needed:
// the caller that wants it — a cron run reconciler wired up much later in
// startup — does not exist yet when the frame lands.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// ErrNoAdoptableTurn means this process had no turn in flight at reconnect (or
// was never a reconnect at all), so there is no outcome to hand over. Callers
// treat it as "nothing to adopt" and fall back to their own bookkeeping — for
// cron that is the interrupted record Phase 0 already writes.
var ErrNoAdoptableTurn = errors.New("cli: no adoptable turn on this process")

// AdoptedEnd is how an adopted turn ended.
type AdoptedEnd string

const (
	// AdoptedEndResult: the CLI delivered a result frame. Outcome.Result is it.
	AdoptedEndResult AdoptedEnd = "result"
	// AdoptedEndCLIExited: readLoop unwound (CLI exit, kill, or a recovered
	// panic) with no result. Outcome.Result is zero — the turn produced no
	// answer, which is a different fact from "the answer was empty".
	AdoptedEndCLIExited AdoptedEnd = "cli_exited"
)

// AdoptedOutcome is what an adopted turn produced. End is what makes it usable:
// a caller deciding between "success" and "interrupted" must not have to infer
// it from an empty Text.
type AdoptedOutcome struct {
	End AdoptedEnd
	// Result is zero when End is AdoptedEndCLIExited.
	Result clievent.SendResult
}

// adoptedTurn latches one turn outcome. Armed at reconnect, resolved exactly
// once by whichever comes first: the late result frame, or readLoop unwinding.
type adoptedTurn struct {
	// armed is read without the mutex by AdoptedOutcome's fast path, and is set
	// before startReadLoop so no resolve can observe it false.
	armed atomic.Bool
	// midTurn: armed by a mid-turn reconnect, not by a replayed result. Set
	// before armed and never cleared, so it outlives the outcome being latched.
	midTurn atomic.Bool
	// resultSeq is the shim seq of the replayed result (0 for a mid-turn
	// latch). Written before armed and never again, so readers that saw armed
	// need no lock.
	resultSeq int64
	done      chan struct{}
	settle    sync.Once

	mu  sync.Mutex
	out AdoptedOutcome
}

// arm marks this process as carrying an adoptable turn; midTurn says the turn
// was still running at reconnect, resultSeq where a replayed result sat. Called
// from SpawnReconnect only, before startReadLoop: arming later would race the
// very frame it exists to catch.
func (a *adoptedTurn) arm(midTurn bool, resultSeq int64) {
	a.done = make(chan struct{})
	a.resultSeq = resultSeq
	a.midTurn.Store(midTurn)
	a.armed.Store(true)
}

// verdictKind is what the drained backlog says about the turn in flight at
// reconnect.
type verdictKind uint8

const (
	verdictIdle     verdictKind = iota // nothing was in flight
	verdictMidTurn                     // the CLI is still working; no result yet
	verdictFinished                    // the backlog ends in a result
	verdictUnknown                     // only turn-neutral frames survived a wrapped ring
)

// replayVerdict is reconnectVerdict's answer; finished is the result frame
// (at shim seq finishedSeq) for verdictFinished only.
type replayVerdict struct {
	kind        verdictKind
	finished    *clievent.Event
	finishedSeq int64
}

// settle turns v into applyReconnectVerdict's arguments, which have no unknown:
// resolve decides it (true = the turn had ended, so idle), and a nil resolve
// leaves it mid-turn, whose stray-result handling recovers if a result comes.
func (v replayVerdict) settle(resolve func(helloSessionID string) bool, helloSessionID string) (midTurn bool, finished *clievent.Event, finishedSeq int64) {
	switch v.kind {
	case verdictMidTurn:
		return true, nil, 0
	case verdictFinished:
		return false, v.finished, v.finishedSeq
	case verdictUnknown:
		return resolve == nil || !resolve(helloSessionID), nil, 0
	}
	return false, nil, 0
}

// applyReconnectVerdict acts on what the drained backlog said. Called from
// SpawnReconnect only, and BEFORE startReadLoop: the read loop can deliver the
// late result before SpawnReconnect returns to its caller, so nothing may be
// attached to that turn after this point (#1778 pinned the same ordering for the
// state flag; the latch inherits the requirement for the same reason).
//
// Three cases, and "nothing was in flight" is one of them: leaving the latch
// unarmed is what lets a caller tell "no turn to adopt" apart from "a turn that
// answered nothing". In all three the CLI ran before this naozhi attached, so
// a later exit is not a startup failure (sawOutput).
func (p *Process) applyReconnectVerdict(midTurn bool, finished *clievent.Event, finishedSeq int64) {
	p.sawOutput.Store(true)
	switch {
	case midTurn:
		p.transition(evReconnectMidTurn)
		p.turn.reconnectedMidTurn.Store(true)
		p.adopted.arm(true, 0)
	case finished != nil:
		// The turn ended while naozhi was down: its result is in the backlog just
		// drained, and this is the only moment it exists in memory — DrainReplay is
		// one-shot per Reconnect, and the caller keeps `replays` only long enough to
		// walk it for linker hints. Latch it now or lose it. State is deliberately
		// left alone: the turn is over, so this is not a mid-turn reconnect and
		// startReadLoop's Ready default is correct.
		p.adopted.arm(false, finishedSeq)
		p.adopted.resolveResult(*finished)
	}
}

// resolveResult latches a result frame. Called from the reconnectedMidTurn CAS
// branch in readLoop, and directly from SpawnReconnect when the replayed backlog
// already ended in a result (the turn finished while naozhi was down).
//
// It must stay inside that CAS branch, because that is where the latch cannot
// steal a result a live Send owns. Two different mechanisms guarantee that, one
// per send path, and both are load-bearing:
//
//   - SendPassthrough (claude's interactive turns): the slot fan-out above the
//     CAS returns as soon as onTurnResult() reports an owner, so a claimed result
//     never reaches here. SendPassthrough has no busy gate — this check is all
//     that separates them.
//   - legacy Send (acp, codex, and claude's cron and non-passthrough turns): it
//     refuses to start while State is Running (ErrProcessBusy), and a mid-turn
//     reconnect stays Running until this very result. A Send that starts once it
//     ends owns the next turn: the CAS has already disarmed the branch, so its
//     result cannot land here. This result still reaches eventCh after Ready;
//     Send drops it as received before its claim.
//
// Moving this call up into deliverEvent would defeat the first; letting a
// mid-turn reconnect leave State anything but Running would defeat the second.
// It also runs after the turn has ended and onTurnDone has fired: a waiter the
// latch wakes may release the process at once, which skips a Running one.
func (a *adoptedTurn) resolveResult(ev clievent.Event) {
	a.resolve(AdoptedOutcome{
		End:    AdoptedEndResult,
		Result: resultFromEvent(ev),
	})
}

// resolveExit latches "ended without a result". Called from readLoop's defer,
// which is the only point every exit passes through (normal EOF, kill, and the
// panic recover above it). Without it a caller that armed the latch and then
// lost the CLI would wait out its whole context for an answer that can never
// come, when "the CLI is gone" was knowable immediately.
func (a *adoptedTurn) resolveExit() {
	a.resolve(AdoptedOutcome{End: AdoptedEndCLIExited})
}

func (a *adoptedTurn) resolve(out AdoptedOutcome) {
	if !a.armed.Load() {
		return
	}
	a.settle.Do(func() {
		a.mu.Lock()
		a.out = out
		a.mu.Unlock()
		close(a.done)
	})
}

// AdoptedTurnPending reports whether this process reconnected mid-turn and its
// outcome has not been latched yet. Not an adoption gate: the late result can
// land before the adopter asks, and that turn is still the one to adopt — gate
// on AdoptedMidTurn instead.
func (p *Process) AdoptedTurnPending() bool {
	if !p.adopted.armed.Load() {
		return false
	}
	select {
	case <-p.adopted.done:
		return false
	default:
		return true
	}
}

// AdoptedMidTurn reports whether this process reconnected to a turn that was
// still running, whether or not its outcome has been latched since. A latch
// armed from a replayed result is excluded: a reconnect replays the shim's
// whole backlog, so that result may be the previous turn's, already delivered.
func (p *Process) AdoptedMidTurn() bool {
	return p.adopted.armed.Load() && p.adopted.midTurn.Load()
}

// AdoptableAfter reports whether the latched turn is the one a Send issued at
// watermark w started (known=false: none was recorded). A mid-turn latch always
// is. A replayed result is only when it sits past w on the same shim, or came
// from a shim started since w; with no watermark it may be the previous turn's.
func (p *Process) AdoptableAfter(w TurnWatermark, known bool) bool {
	if p.AdoptedMidTurn() {
		return true
	}
	if !known || !p.adopted.armed.Load() || p.link.shimPID <= 0 {
		return false
	}
	return w.ShimPID != p.link.shimPID || p.adopted.resultSeq > w.Seq
}

// AdoptedOutcome hands over the outcome of the turn that was in flight at
// reconnect, for a caller that did not issue the Send. Returns immediately when
// the outcome is already latched (including the replay case, where it was
// latched during SpawnReconnect itself), and ErrNoAdoptableTurn when there is
// no adoptable turn — never a bare zero value, so "no turn" cannot be mistaken
// for "a turn that answered nothing".
//
// The outcome stays available after it is read: this is a latch, not a queue.
// Reading it twice is not an error, and two readers both get the same answer.
func (p *Process) AdoptedOutcome(ctx context.Context) (AdoptedOutcome, error) {
	if !p.adopted.armed.Load() {
		return AdoptedOutcome{}, ErrNoAdoptableTurn
	}
	select {
	case <-p.adopted.done:
	case <-ctx.Done():
		return AdoptedOutcome{}, ctx.Err()
	}
	p.adopted.mu.Lock()
	defer p.adopted.mu.Unlock()
	return p.adopted.out, nil
}
