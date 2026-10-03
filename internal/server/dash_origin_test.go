package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/turn"
)

// TestDashAdmission_ReleasesWhenTheTurnPanics pins dashAdmission's deferred
// release: a turn that panics still frees its TrackSend slot, so Shutdown's
// drain returns instead of waiting forever on the owner loop or detached turn
// that panicked.
func TestDashAdmission_ReleasesWhenTheTurnPanics(t *testing.T) {
	for _, mode := range []string{"collect", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: mode})
			turns := h.session(parityKey, mode == "passthrough")
			ws := h.ws()
			ws.send("w1", "first")
			turns.turn(t, "turn", parityOutcome{Panic: "parity: turn panic"})
			ws.waitFor(t, "panic ack", func(f parityFrame) bool { return f.Type == "send_ack" && f.Status == "error" })
			drained := make(chan struct{})
			go func() {
				h.engine().drain()
				close(drained)
			}()
			select {
			case <-drained:
			case <-time.After(parityWait):
				t.Fatal("drain did not return: the panicked turn kept its TrackSend slot")
			}
		})
	}
}

// TestWSOrigin_EverySendInAMergedTurnIsAnswered pins the WS sink as one per
// send id: two sends from one tab merged into one failed turn each get their
// own error ack. A sink per connection would make the second a Mate of the
// first and leave its bubble up.
func TestWSOrigin_EverySendInAMergedTurnIsAnswered(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	ws.send("w3", "third")
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "merged drain turn", parityOutcome{Err: errParityBoom}); !strings.Contains(c.Text, "second") || !strings.Contains(c.Text, "third") {
		t.Fatalf("drain turn = %q, want both queued sends merged", c.Text)
	}
	h.waitEngineIdle()
	var ids []string
	for _, a := range ws.errorAcks() {
		ids = append(ids, a.ID)
	}
	if slices.Sort(ids); strings.Join(ids, ",") != "w2,w3" {
		t.Fatalf("error acks went to %q, want w2 and w3", ids)
	}
}

// TestDashOrigins_ObserverHearsNothing: an owner whose sink is not in the
// batch observes the turn and, on the dashboard, says nothing about it (the
// row-12 drain panic must not reach the finished first send, and an HTTP
// owner must not broadcast a failure that belongs to other requests).
func TestDashOrigins_ObserverHearsNothing(t *testing.T) {
	hub, _ := newTestHub(t, "")
	t.Cleanup(hub.Shutdown)
	c, _ := newCapturedClient(t, hub)
	for name, o := range map[string]turn.Origin{
		"ws":   hub.engine.wsOrigin(c, "w1", "k"),
		"http": hub.engine.httpOrigin("k"),
	} {
		if d := o.Begin(context.Background(), turn.TurnInfo{Role: turn.RoleObserver, Primary: true}); d != nil {
			t.Errorf("%s origin as Observer opened a delivery", name)
		}
		if d := o.Begin(context.Background(), turn.TurnInfo{Role: turn.RoleHead}); d == nil || d.Blocking() {
			t.Errorf("%s origin as Head: delivery %v, want a non-blocking one", name, d)
		}
	}
	if a, b := hub.engine.httpOrigin("k").Sink(), hub.engine.httpOrigin("k").Sink(); a != b {
		t.Errorf("two HTTP sends on one key have sinks %q and %q, want one", a, b)
	}
	if a, b := hub.engine.wsOrigin(c, "w1", "k").Sink(), hub.engine.wsOrigin(c, "w2", "k").Sink(); a == b {
		t.Errorf("two WS sends share sink %q, want one per send id", a)
	}
}
