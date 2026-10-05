package cli

import (
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
)

func newUnownedTurnTestProcess() (*Process, *atomic.Int32) {
	p := &Process{
		eventLog: ring.NewEventLog(16),
		caps:     Caps{Replay: true},
		eventCh:  make(chan clievent.Event, 16),
		killCh:   make(chan struct{}),
	}
	p.turn.state = StateReady
	var done atomic.Int32
	p.turn.onTurnDone = func() { done.Add(1) }
	return p, &done
}

func dispatchAll(p *Process, evs ...clievent.Event) {
	for _, ev := range evs {
		p.dispatchProtocolEvent(ev, slog.New(slog.DiscardHandler))
	}
}

var (
	evInit          = clievent.Event{Type: "system", SubType: "init", SessionID: "s1"}
	evResultSuccess = clievent.Event{Type: "result", SubType: "success", SessionID: "s1"}
	evResultAborted = clievent.Event{Type: "result", SubType: "error_during_execution", SessionID: "s1"}
)

// A turn the CLI starts itself (a background task-notification after the
// owning turn already ended) must return to Ready when its result lands;
// otherwise the session is parked in Running and later reaped as stuck.
func TestDispatch_UnownedTurnResultReturnsToReady(t *testing.T) {
	for _, res := range []clievent.Event{evResultSuccess, evResultAborted} {
		t.Run(res.SubType, func(t *testing.T) {
			p, done := newUnownedTurnTestProcess()
			dispatchAll(p, evInit)
			if got := p.State(); got != StateRunning {
				t.Fatalf("after init state = %v, want Running", got)
			}
			dispatchAll(p, res)
			if got := p.State(); got != StateReady {
				t.Fatalf("after unclaimed result state = %v, want Ready", got)
			}
			if done.Load() != 1 {
				t.Fatalf("onTurnDone fired %d times, want 1", done.Load())
			}
		})
	}
}

func TestDispatch_UnownedTurnRepeatsAcrossNotifications(t *testing.T) {
	p, done := newUnownedTurnTestProcess()
	dispatchAll(p, evInit, evResultSuccess, evInit, evResultSuccess)
	if got := p.State(); got != StateReady {
		t.Fatalf("state = %v, want Ready", got)
	}
	if done.Load() != 2 {
		t.Fatalf("onTurnDone fired %d times, want 2", done.Load())
	}
}

// A Send-owned turn is ended by Send's defer; a result must not end it early
// or a second Send could start before the first returns.
func TestDispatch_SendOwnedTurnStaysRunningOnResult(t *testing.T) {
	p, done := newUnownedTurnTestProcess()
	p.transition(evSendBegin)
	dispatchAll(p, evInit, evResultSuccess)
	if got := p.State(); got != StateRunning {
		t.Fatalf("state = %v, want Running (Send owns the end)", got)
	}
	if done.Load() != 0 {
		t.Fatalf("onTurnDone fired %d times, want 0", done.Load())
	}
}

// A queued passthrough slot means the CLI's next turn is already on its way.
func TestDispatch_UnownedTurnWithPendingSlotStaysRunning(t *testing.T) {
	p, done := newUnownedTurnTestProcess()
	dispatchAll(p, evInit)
	p.slots.pending = append(p.slots.pending, &sendSlot{id: 1, uuid: "u1",
		resultCh: make(chan *clievent.SendResult, 1), errCh: make(chan error, 1)})
	dispatchAll(p, evResultSuccess)
	if got := p.State(); got != StateRunning {
		t.Fatalf("state = %v, want Running", got)
	}
	if done.Load() != 0 {
		t.Fatalf("onTurnDone fired %d times, want 0", done.Load())
	}
}

// A result after the reconnect path already ended the turn must not fire
// onTurnDone a second time through the unowned path.
func TestDispatch_ReconnectMidTurnResultEndsOnce(t *testing.T) {
	p, done := newUnownedTurnTestProcess()
	p.turn.state = StateSpawning
	p.turn.reconnectedMidTurn.Store(true)
	p.transition(evReconnectMidTurn)
	dispatchAll(p, evResultSuccess)
	if got := p.State(); got != StateReady {
		t.Fatalf("state = %v, want Ready", got)
	}
	if done.Load() != 1 {
		t.Fatalf("onTurnDone fired %d times, want 1", done.Load())
	}
}

// When the turn ends, its result must already be on eventCh so a Send that
// claims Ready next drains it instead of taking it as its own answer.
func TestDispatch_UnownedResultQueuedBeforeReady(t *testing.T) {
	for _, res := range []clievent.Event{evResultSuccess, evResultAborted} {
		t.Run(res.SubType, func(t *testing.T) {
			p, _ := newUnownedTurnTestProcess()
			dispatchAll(p, evInit)
			<-p.eventCh // the init frame
			var queuedAtReady bool
			p.turn.onTurnDone = func() { queuedAtReady = len(p.eventCh) == 1 }
			dispatchAll(p, res)
			if !queuedAtReady {
				t.Fatal("state reached Ready before the result was queued on eventCh")
			}
		})
	}
}
