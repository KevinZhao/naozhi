package cli

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// A legacy Send (collect mode, cron) on claude waits for the aborted turn's
// result after /stop; dropping it leaves Send blocked until the no-output
// watchdog and reports the abort as a plain success.
func TestLegacySend_AbortedTurnReturnsItsResult(t *testing.T) {
	for _, tc := range []struct {
		name  string
		abort func(*testing.T, *passthroughShim)
	}{
		{"Interrupt", func(_ *testing.T, sh *passthroughShim) { sh.proc.Interrupt() }},
		{"InterruptViaControl", func(t *testing.T, sh *passthroughShim) {
			if err := sh.proc.InterruptViaControl(); err != nil {
				t.Fatalf("InterruptViaControl: %v", err)
			}
			if in := sh.expectWrite(t, 2*time.Second); in.Type != "control_request" {
				t.Fatalf("stdin write = %q, want the control_request", in.Type)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := newPassthroughShim(t)
			sh.proc.turn.state = StateReady
			t.Cleanup(sh.close)
			go sh.proc.readLoop()

			type sendOut struct {
				res *clievent.SendResult
				err error
			}
			out := make(chan sendOut, 1)
			go func() {
				res, err := sh.proc.Send(context.Background(), "long job", nil, nil)
				out <- sendOut{res, err}
			}()
			sh.expectWrite(t, 2*time.Second)
			sh.emitInit("s1")
			tc.abort(t, sh)
			sh.srv.SendStdout(abortedResult)

			select {
			case o := <-out:
				if o.err != nil {
					t.Fatalf("Send: %v", o.err)
				}
				if r := o.res; !r.Aborted || r.SubType != "error_during_execution" || !r.IsError {
					t.Errorf("Aborted, SubType, IsError = %v, %q, %v; want true, error_during_execution, true",
						r.Aborted, r.SubType, r.IsError)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("Send still blocked after the aborted result (state=%v)", sh.proc.State())
			}
			if got := sh.proc.State(); got != StateReady {
				t.Errorf("state after Send = %v, want Ready", got)
			}
		})
	}
}

// A reconnect that lands mid-turn hands the turn's end to the result frame;
// an aborted result ends it like any other, and is logged once.
func TestDispatch_ReconnectMidTurnAbortedResultEndsTurn(t *testing.T) {
	p, done := newUnownedTurnTestProcess()
	p.turn.state = StateSpawning
	p.turn.reconnectedMidTurn.Store(true)
	p.transition(evReconnectMidTurn)
	dispatchAll(p, evResultAborted)
	if got := p.State(); got != StateReady {
		t.Fatalf("state = %v, want Ready", got)
	}
	if done.Load() != 1 {
		t.Fatalf("onTurnDone fired %d times, want 1", done.Load())
	}
	var results int
	for _, e := range p.eventLog.EntriesSince(0) {
		if e.Type == "result" {
			results++
		}
	}
	if results != 1 {
		t.Errorf("result logged %d times, want 1", results)
	}
}

// An adopter (cron after a restart) reads an aborted turn from the latch as
// what it was: a result, not a CLI exit and not a wait that never ends.
func TestAdoptedTurn_AbortedResultIsLatched(t *testing.T) {
	p, _ := newUnownedTurnTestProcess()
	armReconnectMidTurn(p)
	dispatchAll(p, evResultAborted)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := p.AdoptedOutcome(ctx)
	if err != nil {
		t.Fatalf("AdoptedOutcome: %v", err)
	}
	if out.End != AdoptedEndResult || out.Result.SubType != "error_during_execution" {
		t.Errorf("outcome = %+v, want End=%q with SubType error_during_execution", out, AdoptedEndResult)
	}
	if got := p.State(); got != StateReady {
		t.Errorf("state = %v, want Ready", got)
	}
}
