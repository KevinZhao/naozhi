package session

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
)

// A Send's result inside the window comes back from EndCostWindow and writes
// no row; the cancelled turn's late result after the window closed is one
// unowned row, and the next window does not carry it.
func TestCostWindow_LateResultAfterTheWindowIsTheSessions(t *testing.T) {
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(&clievent.SendResult{Text: "a", CostUSD: 0.4})}
	s, ledger := newLedgerSession(t, "cron:job1", proc)
	hooked := &hookedTestProcess{TestProcess: proc}
	s.storeProcess(hooked)
	bookUnownedResults(s, hooked)

	s.BeginCostWindow()
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	run1 := s.EndCostWindow()
	hooked.unownedHook.fn(clievent.SendResult{CostUSD: 0.6, SessionID: "sid-1"})
	s.BeginCostWindow()
	run2 := s.EndCostWindow()

	if !approxEq(run1.USD, 0.4) || run2.USD != 0 {
		t.Fatalf("window increments = %v, %v; want 0.4 then 0", run1.USD, run2.USD)
	}
	ents := allEntries(t, ledger)
	if len(ents) != 1 || !approxEq(ents[0].Amount, 0.2) || !strings.HasPrefix(ents[0].RunID, "unowned:sid-1:") {
		t.Fatalf("entries = %+v, want one unowned row of the late 0.2", ents)
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 0.6) {
		t.Fatalf("costSpent = %v, want 0.6", got)
	}
}

// Readings racing the window's open and close are each booked exactly once:
// the window increments plus the session rows add up to the final cumulative.
func TestCostWindow_RacingReadingsBookedOnce(t *testing.T) {
	const bookers, perBooker = 4, 40
	s, ledger := newLedgerSession(t, "cron:race", &TestProcess{AliveVal: true})
	var next atomic.Int64
	var wg sync.WaitGroup
	for range bookers {
		wg.Go(func() {
			for range perBooker {
				s.accountTurnCost(&clievent.SendResult{CostUSD: float64(next.Add(1))}, "r")
			}
		})
	}
	var windowed float64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			s.BeginCostWindow()
			windowed += s.EndCostWindow().USD
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-done

	ledger.Close()
	ents, err := ledger.Entries(costledger.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}, 10*bookers*perBooker)
	if err != nil {
		t.Fatal(err)
	}
	var rows float64
	for _, e := range ents {
		rows += e.Amount
	}
	if want := float64(bookers * perBooker); windowed+rows != want {
		t.Fatalf("window %v + rows %v = %v, want the final cumulative %v", windowed, rows, windowed+rows, want)
	}
}

// A process-end partial booked with the window open is its own row and adds
// to costSpent, but the window never carries the estimate.
func TestCostWindow_PartialIsNeverTheWindows(t *testing.T) {
	s, ledger := newLedgerSession(t, "cron:partial", &TestProcess{AliveVal: true})
	ledger.Rates().Observe(costledger.ModelDelta{Model: "claude-opus-5-5", CostUSD: 1, Tokens: costledger.Tokens{Output: 1000}})
	s.BeginCostWindow()
	s.bookPartialUsage(clievent.ShadowUsage{Models: []clievent.ShadowModel{{Model: "claude-opus-5-5", Output: 500}}}, "end:sid:1")
	inc := s.EndCostWindow()

	if inc.USD != 0 || len(inc.Models) != 0 {
		t.Fatalf("window increment = %+v, want none of the partial", inc)
	}
	ents := allEntries(t, ledger)
	if len(ents) != 1 || ents[0].Kind != costledger.KindPartial || !approxEq(ents[0].Amount, 0.5) {
		t.Fatalf("entries = %+v, want one 0.5 partial", ents)
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 0.5) {
		t.Fatalf("costSpent = %v, want 0.5", got)
	}
}

// Spend a replaced session forwards is not the held session's to report, and
// the successor's own window does not collect it either: it is a row.
func TestCostWindow_ForwardedSpendIsARow(t *testing.T) {
	old, ledger := newLedgerSession(t, "cron:fwd", &TestProcess{AliveVal: true})
	fresh := &ManagedSession{key: old.key, costAcct: old.costAcct}
	old.BeginCostWindow()
	fresh.BeginCostWindow()
	linkSuccessor(old, fresh, old.CostTotals())

	old.accountTurnCost(&clievent.SendResult{CostUSD: 0.7}, "run-1")

	if inc := old.EndCostWindow(); inc.USD != 0 {
		t.Fatalf("held window = %+v, want nothing: the spend was forwarded", inc)
	}
	if inc := fresh.EndCostWindow(); inc.USD != 0 {
		t.Fatalf("successor window = %+v, want nothing: forwarded spend is not its result", inc)
	}
	if ents := allEntries(t, ledger); len(ents) != 1 || !approxEq(ents[0].Amount, 0.7) {
		t.Fatalf("entries = %+v, want one row of the forwarded 0.7", ents)
	}
	if got := fresh.CostTotals().USD; !approxEq(got, 0.7) {
		t.Fatalf("successor spend = %v, want 0.7", got)
	}
}

// kiro and codex meter through the same gate: credits reported inside the
// window come back from it, those after it are a metering row.
func TestCostWindow_MeteringFollowsTheWindow(t *testing.T) {
	proc := &TestProcess{AliveVal: true}
	proc.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
		return &clievent.SendResult{Text: "ok"}, nil
	}
	s, ledger := newLedgerSession(t, "cron:kiro", proc)
	s.SetBackend("kiro")

	s.BeginCostWindow()
	proc.MeteringVal = []clievent.MeteringEntry{{Value: 2, Unit: "credit"}}
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	inc := s.EndCostWindow()
	proc.MeteringVal = []clievent.MeteringEntry{{Value: 5, Unit: "credit"}}
	s.accountTurnCost(&clievent.SendResult{}, "late")

	if inc.Metered[costledger.UnitCredits] != 2 {
		t.Fatalf("window increment = %+v, want 2 credits", inc)
	}
	ents := allEntries(t, ledger)
	if len(ents) != 1 || ents[0].Kind != costledger.KindMetering || ents[0].Amount != 3 {
		t.Fatalf("entries = %+v, want one metering row of the late 3 credits", ents)
	}
}

// EndCostWindow without an open window returns nothing, and a reopened window
// starts empty while the earlier window's spend becomes a session row.
func TestCostWindow_EndIsIdempotentAndBeginStartsEmpty(t *testing.T) {
	s, ledger := newLedgerSession(t, "cron:idem", &TestProcess{AliveVal: true})
	if inc := s.EndCostWindow(); inc.USD != 0 {
		t.Fatalf("End with no window = %+v", inc)
	}
	s.BeginCostWindow()
	s.accountTurnCost(&clievent.SendResult{CostUSD: 1}, "r")
	s.BeginCostWindow()
	s.accountTurnCost(&clievent.SendResult{CostUSD: 1.5}, "r")
	if inc := s.EndCostWindow(); !approxEq(inc.USD, 0.5) {
		t.Fatalf("reopened window = %+v, want only the 0.5 after the reopen", inc)
	}
	if inc := s.EndCostWindow(); inc.USD != 0 {
		t.Fatalf("second End = %+v, want zero", inc)
	}
	if ents := allEntries(t, ledger); len(ents) != 1 || ents[0].Source != costledger.SourceSession || !approxEq(ents[0].Amount, 1) {
		t.Fatalf("entries = %+v, want one session row of the earlier window's 1.0", ents)
	}
}
