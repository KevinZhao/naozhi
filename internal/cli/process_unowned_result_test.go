package cli

import (
	"log/slog"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
)

// These pin the cost half of an unowned turn (onUnownedResult); the state half
// and its result-before-Ready ordering are in process_readloop_unowned_turn_test.go.

// unownedFixture is a replay-backend process in Ready with both hooks
// recorded, fed events through dispatchProtocolEvent as readLoop would.
type unownedFixture struct {
	p       *Process
	done    int
	results []clievent.SendResult
}

func newUnownedFixture() *unownedFixture {
	f := &unownedFixture{p: &Process{
		eventLog: ring.NewEventLog(16),
		caps:     Caps{Replay: true},
		eventCh:  make(chan clievent.Event, 4),
		killCh:   make(chan struct{}),
	}}
	f.p.turn.state = StateReady
	f.p.SetOnTurnDone(func() { f.done++ })
	f.p.SetOnUnownedResult(func(r clievent.SendResult) { f.results = append(f.results, r) })
	return f
}

func (f *unownedFixture) feed(evs ...clievent.Event) {
	for _, ev := range evs {
		f.p.dispatchProtocolEvent(ev, slog.New(slog.DiscardHandler))
	}
}

var (
	initEv   = clievent.Event{Type: "system", SubType: "init", SessionID: "s1"}
	resultEv = clievent.Event{Type: "result", SubType: "success", SessionID: "s1", CostUSD: 12.5,
		ModelUsage: map[string]clievent.ModelUsage{"claude-opus-5-5[1m]": {OutputTokens: 40, CostUSD: 12.5}}}
)

// #3096: a turn the CLI starts itself (a background-task notification) is
// moved to Running by its system/init. Its result reached no Send, nothing
// moved it back, and Cleanup's stuck check killed the idle session 2×
// TotalTimeout later; its cost waited for an owned result that a dead process
// never delivers. The result must end the turn and hand its cost over.
func TestUnownedTurn_ResultEndsTheTurnAndHandsOverItsCost(t *testing.T) {
	f := newUnownedFixture()
	f.feed(initEv)
	if got := f.p.State(); got != StateRunning {
		t.Fatalf("after system/init state = %v, want Running", got)
	}
	f.feed(resultEv)

	if got := f.p.State(); got != StateReady {
		t.Errorf("after the unowned result state = %v, want Ready (it stayed Running until a stuck kill)", got)
	}
	if f.done != 1 {
		t.Errorf("onTurnDone fired %d times, want 1 so the dashboard leaves the running spinner", f.done)
	}
	if len(f.results) != 1 || f.results[0].CostUSD != 12.5 || f.results[0].ModelUsage["claude-opus-5-5[1m]"].OutputTokens != 40 {
		t.Fatalf("unowned results = %+v, want the frame's cumulative cost and model rows", f.results)
	}
}

// A Send that claimed the turn owns its result even with no passthrough slot:
// Send ends the turn and its finishRun books the cost, so neither happens here.
func TestUnownedTurn_SendOwnedResultIsLeftToTheSend(t *testing.T) {
	f := newUnownedFixture()
	if _, claimed := f.p.transition(evSendBegin); !claimed {
		t.Fatal("setup: Send could not claim the turn")
	}
	f.feed(initEv, resultEv)

	if got := f.p.State(); got != StateRunning {
		t.Errorf("state = %v, want Running until the Send returns", got)
	}
	if f.done != 0 || len(f.results) != 0 {
		t.Errorf("onTurnDone %d / unowned results %d, want 0 / 0 for a Send-owned turn", f.done, len(f.results))
	}

	// Once that Send has returned, a later CLI-started turn is unowned again.
	f.p.transition(evSendEnd)
	f.feed(initEv, resultEv)
	if got := f.p.State(); got != StateReady || len(f.results) != 1 {
		t.Errorf("after Send returned: state %v, unowned results %d; want Ready and 1", got, len(f.results))
	}
}

// Passthrough messages still queued for the next turn keep the process
// Running (the next system/init continues), but the finished turn's cost is
// still nobody else's to book.
func TestUnownedTurn_QueuedPassthroughKeepsRunningButBooksCost(t *testing.T) {
	f := newUnownedFixture()
	f.p.slots.pending = []*sendSlot{{}}
	f.feed(initEv, resultEv)

	if got := f.p.State(); got != StateRunning {
		t.Errorf("state = %v, want Running while passthrough messages are queued", got)
	}
	if f.done != 0 {
		t.Errorf("onTurnDone fired %d times, want 0 (the turn continues)", f.done)
	}
	if len(f.results) != 1 {
		t.Errorf("unowned results = %d, want 1", len(f.results))
	}
}

// A reconnect's in-flight turn has its own end-of-turn path (the adopted
// latch); this hook must not also end it.
func TestUnownedTurn_ReconnectMidTurnIsLeftToTheAdoptedPath(t *testing.T) {
	f := newUnownedFixture()
	f.p.turn.state = StateRunning
	f.p.turn.reconnectedMidTurn.Store(true)
	f.feed(resultEv)

	if len(f.results) != 0 {
		t.Errorf("unowned results = %d, want 0 on the reconnect path", len(f.results))
	}
	if got := f.p.State(); got != StateReady || f.done != 1 {
		t.Errorf("state %v, onTurnDone %d; want the adopted path's Ready and exactly 1", got, f.done)
	}
}
