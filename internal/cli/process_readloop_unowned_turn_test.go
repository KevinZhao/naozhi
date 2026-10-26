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
