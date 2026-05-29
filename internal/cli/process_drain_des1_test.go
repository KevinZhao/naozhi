package cli

import (
	"context"
	"testing"
	"time"
)

// TestDrainStaleEvents_DES1_HoldFreshWaitForResult pins R29-DES1 (#773).
//
// Event ordering inside the interrupted-settle window: a fresh (post-cutoff,
// new-turn) event arrives BEFORE the interrupted turn's result event. The
// pre-fix code re-enqueued the fresh event and `goto drain` immediately,
// abandoning the settle wait — so an interrupted result still in flight could
// arrive after drain's non-blocking sweep emptied the channel and leak into
// the next turn's output.
//
// The fix holds the fresh event aside and keeps waiting for the interrupted
// result. This test asserts the post-condition the invariant guarantees:
//   - the interrupted result is absorbed (NOT left in eventCh), and
//   - the fresh new-turn event is preserved (re-enqueued for the live
//     consumer).
func TestDrainStaleEvents_DES1_HoldFreshWaitForResult(t *testing.T) {
	t.Parallel()

	p := &Process{
		eventCh: make(chan Event, 8),
		done:    make(chan struct{}), // open: isChanAlive must report true
	}
	p.interrupted.Store(true)
	p.interruptedRun.Store(true)

	// Both events post-date the cutoff captured at drainStaleEvents entry
	// (cutoff := time.Now()), but only the result triggers the settle exit.
	// Ordering [fresh, result] is what the old push-back path mishandled:
	// reading `fresh` used to short-circuit out of the settle window.
	future := time.Now().Add(time.Hour)
	freshEv := Event{Type: "assistant", SessionID: "new-turn", recvAt: future}
	resultEv := Event{Type: "result", SessionID: "interrupted", recvAt: future}
	p.eventCh <- freshEv
	p.eventCh <- resultEv

	if err := p.drainStaleEvents(context.Background()); err != nil {
		t.Fatalf("drainStaleEvents() error = %v", err)
	}

	// The interrupted result must have been consumed by the settle window;
	// only the fresh new-turn event should remain for the live consumer.
	var remaining []Event
	for {
		select {
		case ev := <-p.eventCh:
			remaining = append(remaining, ev)
			continue
		default:
		}
		break
	}

	if len(remaining) != 1 {
		t.Fatalf("eventCh after drain has %d events, want 1 (only the held fresh event); got %+v", len(remaining), remaining)
	}
	if remaining[0].Type == "result" {
		t.Fatalf("interrupted result leaked into next turn: %+v", remaining[0])
	}
	if remaining[0].SessionID != "new-turn" {
		t.Fatalf("re-enqueued event = %+v, want the held new-turn event", remaining[0])
	}
}
