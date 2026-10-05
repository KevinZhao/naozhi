package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// #3322: a result that arrives after its Send gave up (an interrupt canceled
// the Send's ctx, a cron deadline fired) reached no booking, while the
// meter's LastResultAt still moved past it, so the process-end window never
// covered that turn either. Such a result must reach onUnownedResult.

func TestUnclaimedResult_AfterSendReturnedIsBooked(t *testing.T) {
	for _, subtype := range []string{"success", "error_during_execution"} {
		t.Run(subtype, func(t *testing.T) {
			f := newUnownedFixture()
			if _, claimed := f.p.transition(evSendBegin); !claimed {
				t.Fatal("setup: Send could not claim the turn")
			}
			f.feed(initEv)
			f.p.transition(evSendEnd) // the Send returned on its ctx
			late := resultEv
			late.SubType = subtype
			f.feed(late)

			if len(f.results) != 1 || f.results[0].CostUSD != 12.5 || f.results[0].ModelUsage["claude-opus-5-5[1m]"].OutputTokens != 40 {
				t.Fatalf("unowned results = %+v, want the late frame's cumulative cost and model rows once", f.results)
			}
			if got := f.p.State(); got != StateReady || f.done != 0 {
				t.Errorf("state %v, onTurnDone %d; want Ready and 0 (no turn of its own ended)", got, f.done)
			}
		})
	}
}

// The Send gave up but its defer has not turned the process Ready yet.
func TestUnclaimedResult_AbandonedSendIsBooked(t *testing.T) {
	f := newUnownedFixture()
	f.p.transition(evSendBegin)
	f.feed(initEv)
	f.p.turn.mu.Lock()
	f.p.turn.sendAbandoned = true
	f.p.turn.mu.Unlock()
	f.feed(resultEv)

	if len(f.results) != 1 {
		t.Fatalf("unowned results = %d, want 1 for an abandoned Send's result", len(f.results))
	}
	if got := f.p.State(); got != StateRunning || f.done != 0 {
		t.Errorf("state %v, onTurnDone %d; want Running and 0 (Send's defer ends the turn)", got, f.done)
	}
	// The next Send owns its own result again.
	f.p.transition(evSendEnd)
	f.p.transition(evSendBegin)
	f.feed(resultEv)
	if len(f.results) != 1 {
		t.Errorf("unowned results = %d after a new Send claimed the turn, want still 1", len(f.results))
	}
}

// Backends without replay (acp, codex, kiro) have no unowned turns, but a
// result after their Send gave up is just as unclaimed.
func TestUnclaimedResult_NonReplayBackend(t *testing.T) {
	f := newUnownedFixture()
	f.p.caps = Caps{}
	f.p.transition(evSendBegin)
	f.feed(resultEv)
	if len(f.results) != 0 {
		t.Fatalf("unowned results = %d, want 0 while a Send owns the turn", len(f.results))
	}
	f.p.transition(evSendEnd)
	f.feed(resultEv)
	if len(f.results) != 1 {
		t.Fatalf("unowned results = %d, want 1 for the result after the Send returned", len(f.results))
	}
	if got := len(f.p.eventCh); got != 2 {
		t.Errorf("eventCh holds %d events, want both results still handed to eventCh", got)
	}
}

// A Send that takes its result off eventCh and returns can turn the process
// Ready before readLoop finishes with that result; the check that decides
// "unclaimed" must not see that Ready. Holding slots.mu parks readLoop just
// after the handoff (settleUnclaimedResult takes it first) while the Send
// returns.
func TestUnclaimedResult_ConsumedResultIsNotBookedAgain(t *testing.T) {
	f := newUnownedFixture()
	f.p.caps = Caps{} // no replay: nothing before the handoff takes slots.mu
	f.p.transition(evSendBegin)
	f.p.slots.mu.Lock()
	fed := make(chan struct{})
	go func() {
		f.feed(resultEv)
		close(fed)
	}()
	<-f.p.eventCh // the Send takes its result
	f.p.transition(evSendEnd)
	f.p.slots.mu.Unlock()
	<-fed

	if len(f.results) != 0 {
		t.Fatalf("unowned results = %d, want 0 for a result its Send consumed", len(f.results))
	}
}

// A result queued on eventCh in the instant its Send gave up is not seen as
// unclaimed by readLoop; the next Send's drain is the last place it surfaces.
func TestUnclaimedResult_DrainBooksStaleResults(t *testing.T) {
	t.Run("plain drain", func(t *testing.T) {
		f := newUnownedFixture()
		f.p.done = make(chan struct{})
		f.p.eventCh <- clievent.Event{Type: "assistant", RecvAt: time.Now()}
		f.p.eventCh <- clievent.Event{Type: "result", SubType: "success", CostUSD: 2, RecvAt: time.Now()}
		if err := f.p.drainStaleEvents(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.results) != 1 || f.results[0].CostUSD != 2 {
			t.Fatalf("unowned results = %+v, want the drained result once", f.results)
		}
	})
	t.Run("interrupt settle", func(t *testing.T) {
		f := newUnownedFixture()
		f.p.done = make(chan struct{})
		f.p.turn.interrupted.Store(true)
		f.p.turn.interruptedRun.Store(true)
		f.p.eventCh <- clievent.Event{Type: "result", SubType: "error_during_execution", CostUSD: 3, RecvAt: time.Now()}
		if err := f.p.drainStaleEvents(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.results) != 1 || f.results[0].CostUSD != 3 {
			t.Fatalf("unowned results = %+v, want the interrupted turn's result once", f.results)
		}
	})
	t.Run("new turn's result is held back", func(t *testing.T) {
		f := newUnownedFixture()
		f.p.done = make(chan struct{})
		f.p.eventCh <- clievent.Event{Type: "result", SubType: "success", CostUSD: 4, RecvAt: time.Now().Add(time.Hour)}
		if err := f.p.drainStaleEvents(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.results) != 0 || len(f.p.eventCh) != 1 {
			t.Fatalf("unowned results %d, eventCh %d; want 0 and the held-back result re-queued", len(f.results), len(f.p.eventCh))
		}
	})
}

// End to end over the shim link: a Send canceled mid-turn returns, then the
// CLI's result for that turn arrives and is booked.
func TestUnclaimedResult_CanceledSendLateResultIsBooked(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	writes := startServerWriteTap(srv)
	booked := make(chan clievent.SendResult, 4)
	p.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })
	p.startReadLoop()
	defer p.Kill()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := p.Send(ctx, "hi", nil, nil)
		errCh <- err
	}()
	awaitWrite(t, writes)
	srv.SendStdout(`{"type":"system","subtype":"init","session_id":"s1"}`)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Send = %v, want context.Canceled", err)
	}
	p.turn.mu.RLock()
	abandoned := p.turn.sendAbandoned
	p.turn.mu.RUnlock()
	if !abandoned {
		t.Error("a Send that gave up did not mark its turn abandoned")
	}

	srv.SendStdout(`{"type":"result","subtype":"success","session_id":"s1","total_cost_usd":1.5}`)
	select {
	case r := <-booked:
		if r.CostUSD != 1.5 {
			t.Fatalf("booked cost = %v, want 1.5", r.CostUSD)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled Send's late result was never booked")
	}
	if p.LastResultAt().IsZero() {
		t.Error("LastResultAt not set by the late result")
	}
}

// A result read before the Send claimed the turn but queued after its drain
// is dropped by the Send's own loop, and booked there.
func TestUnclaimedResult_SendDropsAndBooksAnEarlierTurnsResult(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	writes := startServerWriteTap(srv)
	booked := make(chan clievent.SendResult, 4)
	p.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })
	p.startReadLoop()
	defer p.Kill()

	before := time.Now()
	type sendOut struct {
		res *clievent.SendResult
		err error
	}
	out := make(chan sendOut, 1)
	go func() {
		res, err := p.Send(context.Background(), "hi", nil, nil)
		out <- sendOut{res, err}
	}()
	awaitWrite(t, writes)
	p.eventCh <- clievent.Event{Type: "result", SubType: "success", CostUSD: 0.5, RecvAt: before}
	srv.SendStdout(`{"type":"result","subtype":"success","session_id":"s1","result":"mine","total_cost_usd":0.75}`)

	o := <-out
	if o.err != nil || o.res.Text != "mine" {
		t.Fatalf("Send = %+v, %v; want its own result", o.res, o.err)
	}
	select {
	case r := <-booked:
		if r.CostUSD != 0.5 {
			t.Fatalf("booked cost = %v, want the earlier turn's 0.5", r.CostUSD)
		}
	default:
		t.Fatal("the earlier turn's result Send dropped was never booked")
	}
	if len(booked) != 0 {
		t.Errorf("%d more results booked, want only the dropped one", len(booked))
	}
}
