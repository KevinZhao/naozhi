package cli

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// #3322: a passthrough caller that stops waiting (its ctx canceled, or the
// bail timer orphaned it) leaves a tombstone, and the result the CLI still
// reports for that turn was dropped with no booking. The head's cost must
// reach onUnownedResult instead.

func newTestSlot(id uint64) *sendSlot {
	return &sendSlot{id: id, resultCh: make(chan *clievent.SendResult, 1), errCh: make(chan error, 1)}
}

// bookingRecorder is an onUnownedResult that may be called from readLoop.
type bookingRecorder struct {
	mu     sync.Mutex
	booked []clievent.SendResult
}

func (r *bookingRecorder) book(res clievent.SendResult) {
	r.mu.Lock()
	r.booked = append(r.booked, res)
	r.mu.Unlock()
}

func (r *bookingRecorder) take() []clievent.SendResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.booked
	r.booked = nil
	return got
}

func TestPassthroughUnclaimed_FanOut(t *testing.T) {
	ev := clievent.Event{Type: "result", SubType: "success", SessionID: "s1", Result: "reply", CostUSD: 2.5}
	for _, tc := range []struct {
		name             string
		canceled         []bool // head first
		wantBooked       bool
		wantDeliveredTo  []bool
		wantFollowerHead bool
	}{
		{"canceled head, live follower", []bool{true, false}, true, []bool{false, true}, true},
		{"live head, canceled follower", []bool{false, true}, false, []bool{true, false}, false},
		{"every owner canceled", []bool{true, true}, true, []bool{false, false}, false},
		{"lone canceled head", []bool{true}, true, []bool{false}, false},
		{"lone live head", []bool{false}, false, []bool{true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Process{}
			var rec bookingRecorder
			p.SetOnUnownedResult(rec.book)
			owners := make([]*sendSlot, len(tc.canceled))
			for i, c := range tc.canceled {
				owners[i] = newTestSlot(uint64(i + 1))
				owners[i].canceled.Store(c)
			}
			p.fanoutTurnResult(owners, ev)

			booked := rec.take()
			if tc.wantBooked && (len(booked) != 1 || booked[0].CostUSD != 2.5) {
				t.Errorf("booked %+v, want the head's 2.5 once", booked)
			}
			if !tc.wantBooked && len(booked) != 0 {
				t.Errorf("booked %+v, want nothing: a live head's finishRun owns the cost", booked)
			}
			for i, s := range owners {
				if got := len(s.resultCh) == 1; got != tc.wantDeliveredTo[i] {
					t.Errorf("owner %d delivered = %v, want %v", i, got, tc.wantDeliveredTo[i])
				}
			}
			if tc.wantFollowerHead {
				if r := <-owners[1].resultCh; r.MergedWithHead != owners[0].id || r.CostUSD != 0 || r.HeadText != "reply" {
					t.Errorf("follower result %+v, want a zero-cost pointer at the head", r)
				}
			}
		})
	}
}

// A result delivered and a caller leaving at the same moment: either the
// caller's abandonSlot takes the result back, or fan-out finds the tombstone
// and books it. Exactly one of the two, every time.
func TestPassthroughUnclaimed_CancelRacingDelivery(t *testing.T) {
	ev := clievent.Event{Type: "result", SubType: "success", CostUSD: 1}
	for i := range 2000 {
		p := &Process{}
		var rec bookingRecorder
		p.SetOnUnownedResult(rec.book)
		slot := newTestSlot(1)
		var ready, wg sync.WaitGroup
		start := make(chan struct{})
		var takenBack *clievent.SendResult
		ready.Add(2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			p.fanoutTurnResult([]*sendSlot{slot}, ev)
		}()
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			takenBack = p.abandonSlot(slot)
		}()
		ready.Wait()
		close(start)
		wg.Wait()

		booked := len(rec.take())
		if takenBack != nil {
			booked++
		}
		if booked != 1 || len(slot.resultCh) != 0 {
			t.Fatalf("iteration %d: result accounted %d times, %d left unread; want exactly once", i, booked, len(slot.resultCh))
		}
	}
}

// awaitSlot's ctx.Done arm books a head result that was delivered but not
// read; a follower's zero-cost pointer is not booked.
func TestPassthroughUnclaimed_CanceledAwaitBooksADeliveredResult(t *testing.T) {
	sh := startWatchdogShim(t, time.Hour, time.Hour, parkedWatchdog)
	var rec bookingRecorder
	sh.proc.SetOnUnownedResult(rec.book)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name       string
		res        clievent.SendResult
		wantBooked int
	}{
		{"head", clievent.SendResult{CostUSD: 3}, 1},
		{"follower", clievent.SendResult{MergedWithHead: 7}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawCancel := false
			for i := range 200 {
				slot := newTestSlot(uint64(i + 1))
				res := tc.res
				slot.resultCh <- &res
				got, err := sh.proc.awaitSlot(canceled, slot)
				booked := len(rec.take())
				switch {
				case err == nil && got == &res:
					if booked != 0 {
						t.Fatalf("iteration %d: result returned and booked %d times", i, booked)
					}
				case errors.Is(err, context.Canceled):
					sawCancel = true
					if booked != tc.wantBooked {
						t.Fatalf("iteration %d: canceled with %d bookings, want %d", i, booked, tc.wantBooked)
					}
				default:
					t.Fatalf("iteration %d: awaitSlot = %v, %v", i, got, err)
				}
			}
			if !sawCancel {
				t.Fatal("ctx.Done arm never chosen in 200 tries")
			}
		})
	}
}

// End to end: a caller canceled after its message was claimed returns, then
// the CLI's result for that turn arrives and is booked.
func TestPassthroughUnclaimed_CanceledSlotLateResultIsBooked(t *testing.T) {
	sh := startWatchdogShim(t, time.Hour, time.Hour, parkedWatchdog)
	booked := make(chan clievent.SendResult, 4)
	sh.proc.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })

	ctx, cancel := context.WithCancel(context.Background())
	uuid, out := sh.sendAsync(t, ctx, "A")
	sh.emitInit("s1")
	sh.emitReplay(uuid, "A")
	cancel()
	if o := waitOut(t, "A", out, 3*time.Second); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", o.err)
	}
	sh.emitResult("s1", "late reply")
	awaitBooked(t, booked, 0.001)
}

// The bail timer orphans a slot whose turn overran; the result that finally
// arrives is booked.
func TestPassthroughUnclaimed_OrphanedSlotLateResultIsBooked(t *testing.T) {
	sh := startWatchdogShim(t, time.Hour, 80*time.Millisecond, parkedWatchdog)
	booked := make(chan clievent.SendResult, 4)
	sh.proc.SetOnUnownedResult(func(r clievent.SendResult) { booked <- r })

	uuid, out := sh.sendAsync(t, context.Background(), "A")
	sh.emitInit("s1")
	sh.emitReplay(uuid, "A")
	if o := waitOut(t, "A", out, 3*time.Second); !errors.Is(o.err, clierr.ErrOrphanedSlot) {
		t.Fatalf("err = %v, want ErrOrphanedSlot", o.err)
	}
	sh.emitResult("s1", "late reply")
	awaitBooked(t, booked, 0.001)
}

// awaitBooked waits for the late result to be booked, then checks no second
// booking of it follows.
func awaitBooked(t *testing.T, booked <-chan clievent.SendResult, wantUSD float64) {
	t.Helper()
	select {
	case r := <-booked:
		if r.CostUSD != wantUSD {
			t.Fatalf("booked cost = %v, want %v", r.CostUSD, wantUSD)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the late result was never booked")
	}
	select {
	case r := <-booked:
		t.Fatalf("the late result was booked twice (second: %+v)", r)
	case <-time.After(150 * time.Millisecond):
	}
}
