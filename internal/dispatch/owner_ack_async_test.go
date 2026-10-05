package dispatch

// #3329: a request that runs at once gets its ⏳ beside its turn, not before
// it, and every clear of that ⏳ waits for the add so it cannot land after.

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// TestAckInFlight_TurnRunsAndClearWaits holds the ⏳ add open: the turn
// still reaches its session and reply, and does not finish (clear) until the
// add has landed. A detached PriorityNow turn is not First, so it clears the
// ⏳ whether or not it landed and must wait all the same.
func TestAckInFlight_TurnRunsAndClearWaits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		first bool
	}{{"owner", true}, {"detached", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &bannerPlatform{replied: make(chan struct{}, 8)}
			p.addGate = make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				runTurnOn(p, p.record, existingSession, answer, tc.first)
			}()
			select {
			case <-p.replied:
			case <-time.After(2 * time.Second):
				t.Fatal("no reply while the ⏳ add was in flight: the turn waited for it")
			}
			select {
			case <-done:
				t.Fatal("turn finished while its ⏳ add was in flight")
			case <-time.After(50 * time.Millisecond):
			}
			close(p.addGate)
			<-done
			if got, want := p.events(), []string{"session", "reply", "add:m1", "remove:m1"}; !slices.Equal(got, want) {
				t.Errorf("events = %v, want %v", got, want)
			}
		})
	}
}

// TestDropped_WaitsForAckInFlight: Dropped clears the ⏳ only after an add
// still in flight.
func TestDropped_WaitsForAckInFlight(t *testing.T) {
	t.Parallel()
	p := &ackOrderPlatform{addGate: make(chan struct{})}
	d := newTestDispatcher(&fakePlatform{})
	d.platforms = map[string]platform.Platform{"fake": p}
	ctx := context.Background()
	o := d.newIMOrigin(reactorMsg("m1", "hi"), slog.Default(), reactorKey, "general", session.AgentOpts{}, imMessage, 2, 0)
	o.Admitted(ctx, turn.AckOwner)
	dropped := make(chan struct{})
	go func() {
		defer close(dropped)
		o.Dropped(ctx, turn.DropReset)
	}()
	select {
	case <-dropped:
		t.Fatal("Dropped returned while the ⏳ add was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(p.addGate)
	<-dropped
	if got, want := p.events(), []string{"add:m1", "remove:m1"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// TestAdmitted_AckOutlivesInboundCtx: a detached request's inbound ctx can
// end once Submit returns; its ⏳ add must still land.
func TestAdmitted_AckOutlivesInboundCtx(t *testing.T) {
	t.Parallel()
	p := &ackOrderPlatform{addGate: make(chan struct{})}
	d := newTestDispatcher(&fakePlatform{})
	d.platforms = map[string]platform.Platform{"fake": p}
	ctx, cancel := context.WithCancel(context.Background())
	o := d.newIMOrigin(reactorMsg("m1", "hi"), slog.Default(), reactorKey, "general", session.AgentOpts{}, imMessage, 2, 0)
	o.Admitted(ctx, turn.AckDetached)
	cancel()
	select {
	case <-o.ackDone:
		t.Fatal("the ⏳ add ended with the inbound ctx")
	case <-time.After(50 * time.Millisecond):
	}
	close(p.addGate)
	if !o.awaitAck() {
		t.Error("awaitAck = false: the ⏳ add died with the inbound ctx")
	}
	if got, want := p.events(), []string{"add:m1"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}
