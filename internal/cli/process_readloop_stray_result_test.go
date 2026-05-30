package cli

import (
	"log/slog"
	"testing"
)

// TestDispatchProtocolEvent_StrayReplayResultLogged is the #1483 regression
// guard: a "result" event on a replay-capable protocol with zero claimed
// owners and SubType != "error_during_execution" (a true stray/reconnect
// result) must still be appended to the EventLog. Previously the no-owner
// branch could be read as skipping the legacy path; this pins that the
// turn-complete entry reaches the dashboard transcript.
func TestDispatchProtocolEvent_StrayReplayResultLogged(t *testing.T) {
	p := &Process{
		eventLog: NewEventLog(8),
		caps:     Caps{Replay: true},
		eventCh:  make(chan Event, 1),
		killCh:   make(chan struct{}),
	}

	// No slots claimed → onTurnResult returns empty owners → stray path.
	ev := Event{Type: "result", SubType: "success", SessionID: "s1"}
	p.dispatchProtocolEvent(ev, slog.New(slog.DiscardHandler))

	entries := p.eventLog.Entries()
	var found bool
	for _, e := range entries {
		if e.Type == "result" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("stray replay result not appended to EventLog; entries=%+v", entries)
	}
}
