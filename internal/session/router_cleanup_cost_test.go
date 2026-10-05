package session

import (
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/datadir"
)

// detachingProc is a shim-backed process whose Detach runs onDetach: what the
// CLI reports between shutdown's store save and the socket closing.
type detachingProc struct {
	*hookedTestProcess
	onDetach func()
}

func (p *detachingProc) Detach() {
	if p.onDetach != nil {
		p.onDetach()
	}
}

const shutdownCostKey = "dashboard:direct:shutdown-cost:general"

func newCostRouter(t *testing.T, storePath string) *Router {
	t.Helper()
	r := NewRouter(RouterConfig{StorePath: storePath, Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	t.Cleanup(r.Shutdown)
	return r
}

// startCostRouter spawns shutdownCostKey on a router persisting to storePath
// and returns the session and its process.
func startCostRouter(t *testing.T, storePath string) (*Router, *ManagedSession, *detachingProc) {
	t.Helper()
	r := newCostRouter(t, storePath)
	proc := &detachingProc{hookedTestProcess: &hookedTestProcess{TestProcess: &TestProcess{AliveVal: true}}}
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return proc, nil }
	s, _, err := r.GetOrCreate(context.Background(), shutdownCostKey, AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s.setSessionID("sid-shutdown")
	return r, s, proc
}

// ledgerTotal sums the USD rows the ledger beside storePath holds.
func ledgerTotal(t *testing.T, storePath string) float64 {
	t.Helper()
	l := costledger.NewStore(datadir.ForStore(storePath).CostRoot(), costledger.Options{})
	defer l.Close()
	ents, err := l.Entries(costledger.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, e := range ents {
		total += e.Amount
	}
	return total
}

// #3428: the CLI survives shutdown, so a result it reports after the store
// save is differenced again by its next result after the restart. Booking it
// before the restart charged that turn twice; it is left to the restart.
func TestShutdown_ReadingAfterTheStoreSaveIsBookedOnce(t *testing.T) {
	late := map[string]func(*ManagedSession, *detachingProc, clievent.SendResult){
		"unowned result": func(_ *ManagedSession, p *detachingProc, res clievent.SendResult) { p.fn(res) },
		"owned turn": func(s *ManagedSession, _ *detachingProc, res clievent.SendResult) {
			s.accountTurnCost(&res, newRunID())
		},
	}
	for name, deliver := range late {
		t.Run(name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "sessions.json")
			r, s, proc := startCostRouter(t, storePath)
			proc.fn(clievent.SendResult{CostUSD: 1})
			proc.onDetach = func() { deliver(s, proc, clievent.SendResult{CostUSD: 3}) }
			r.Shutdown()

			r2 := newCostRouter(t, storePath)
			restored, ok := lookupT(r2, shutdownCostKey)
			if !ok {
				t.Fatal("session not restored")
			}
			reattached := &hookedTestProcess{TestProcess: &TestProcess{AliveVal: true}}
			restored.storeProcess(reattached)
			bookUnownedResults(restored, reattached)
			reattached.fn(clievent.SendResult{CostUSD: 4}) // the same CLI's next result
			if got := loadTotalCost(&restored.costSpent); !approxEq(got, 4) {
				t.Errorf("restored costSpent = %v, want 4", got)
			}
			r2.Shutdown()
			if got := ledgerTotal(t, storePath); !approxEq(got, 4) {
				t.Fatalf("ledger total = %v, want 4 (the CLI's cumulative, each turn once)", got)
			}
		})
	}
}

// A frozen router books nothing on either path and leaves the baseline alone.
func TestAccountCost_FrozenBooksNothing(t *testing.T) {
	proc := &hookedTestProcess{TestProcess: &TestProcess{AliveVal: true}}
	s, ledger := newLedgerSession(t, "dashboard:direct:frozen:general", proc.TestProcess)
	s.storeProcess(proc)
	bookUnownedResults(s, proc)
	proc.fn(clievent.SendResult{CostUSD: 1})

	if !s.costAcct.freeze(time.Second) {
		t.Fatal("freeze timed out with no booking running")
	}
	proc.fn(clievent.SendResult{CostUSD: 3})
	if inc := s.accountTurnCost(&clievent.SendResult{CostUSD: 3}, "run-1"); inc != 0 {
		t.Errorf("frozen accountTurnCost = %v, want 0", inc)
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 1) {
		t.Errorf("costSpent = %v, want 1", got)
	}
	if got := loadTotalCost(&s.lastCumulativeCost); !approxEq(got, 1) {
		t.Errorf("lastCumulativeCost = %v, want 1 (the saved baseline)", got)
	}
	if ents := allEntries(t, ledger); len(ents) != 1 {
		t.Fatalf("entries = %+v, want only the pre-freeze row", ents)
	}
}

// Readings racing shutdown each land either in the saved baseline (and the
// ledger) or nowhere: the saved baseline and spend equal the ledger total.
func TestShutdown_ReadingsRacingTheSaveMatchTheSavedBaseline(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	r, _, proc := startCostRouter(t, storePath)
	var wg sync.WaitGroup
	started := make(chan struct{})
	wg.Add(1)
	go func() { // fewer readings than the ledger queue holds, so none is dropped
		defer wg.Done()
		for i := 1; i <= 2000; i++ {
			proc.fn(clievent.SendResult{CostUSD: float64(i) * 0.25})
			if i == 1 {
				close(started)
			}
		}
	}()
	<-started
	r.Shutdown()
	wg.Wait()

	entry := loadStore(storePath)[shutdownCostKey]
	if entry == nil {
		t.Fatal("session not saved")
	}
	total := ledgerTotal(t, storePath)
	if !approxEq(total, entry.LastCumulativeCost) || !approxEq(total, entry.CostSpent) {
		t.Fatalf("ledger total %v, saved baseline %v, saved spend %v: want all equal",
			total, entry.LastCumulativeCost, entry.CostSpent)
	}
}

// freeze waits for a reading admitted before it, so the save that follows
// reads the baseline that reading advanced.
func TestCostFreeze_WaitsForAnAdmittedBooking(t *testing.T) {
	proc := &hookedTestProcess{TestProcess: &TestProcess{AliveVal: true}}
	s, _ := newLedgerSession(t, "dashboard:direct:freeze-wait:general", proc.TestProcess)
	s.storeProcess(proc)
	bookUnownedResults(s, proc)

	s.costMu.Lock() // the reading is admitted, then blocks before booking
	done := make(chan struct{})
	go func() { defer close(done); proc.fn(clievent.SendResult{CostUSD: 2}) }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		s.costAcct.books.mu.Lock()
		n := s.costAcct.books.n
		s.costAcct.books.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			s.costMu.Unlock()
			t.Fatal("reading never admitted")
		}
		runtime.Gosched()
	}
	if s.costAcct.freeze(20 * time.Millisecond) {
		s.costMu.Unlock()
		t.Fatal("freeze reported done while a booking was running")
	}
	s.costMu.Unlock()
	if !s.costAcct.freeze(5 * time.Second) {
		t.Fatal("freeze timed out after the booking could finish")
	}
	<-done
	if got := loadTotalCost(&s.lastCumulativeCost); !approxEq(got, 2) {
		t.Fatalf("lastCumulativeCost = %v, want 2 (the admitted reading)", got)
	}
}
