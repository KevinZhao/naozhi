package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/datadir"
)

// crashAfterSave books cum=1 on a fresh router, saves the store, then runs
// late and flushes the ledger without a shutdown: what a killed naozhi
// leaves on disk. Returns the store path.
func crashAfterSave(t *testing.T, late func(s *ManagedSession, proc *detachingProc)) string {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	r, s, proc := startCostRouter(t, storePath)
	proc.fn(clievent.SendResult{CostUSD: 1})
	r.saveIfDirty()
	if e := loadStore(storePath)[shutdownCostKey]; e == nil || !approxEq(e.CostSpent, 1) {
		t.Fatalf("saved entry = %+v, want cost_spent 1", e)
	}
	late(s, proc)
	r.runs.cost.ledger.Close()
	return storePath
}

// reattach restores storePath on a new router and feeds the surviving CLI's
// next cumulative reading to the restored session.
func reattach(t *testing.T, storePath string, cum float64) (*Router, *ManagedSession) {
	t.Helper()
	r := newCostRouter(t, storePath)
	s, ok := lookupT(r, shutdownCostKey)
	if !ok {
		t.Fatal("session not restored")
	}
	proc := &hookedTestProcess{TestProcess: &TestProcess{AliveVal: true}}
	s.storeProcess(proc)
	bookUnownedResults(s, proc)
	proc.fn(clievent.SendResult{CostUSD: cum})
	return r, s
}

// A turn booked after the last store save is in the ledger but not in the
// saved baseline; the reattached CLI's next result must not book it again.
func TestRestore_CrashAfterSaveBooksTheGapOnce(t *testing.T) {
	storePath := crashAfterSave(t, func(_ *ManagedSession, proc *detachingProc) {
		proc.fn(clievent.SendResult{CostUSD: 3})
	})
	r2, s := reattach(t, storePath, 4)
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 4) {
		t.Errorf("restored costSpent = %v, want 4", got)
	}
	r2.Shutdown()
	if got := ledgerTotal(t, storePath); !approxEq(got, 4) {
		t.Fatalf("ledger total = %v, want 4 (the CLI's cumulative, each turn once)", got)
	}
}

// A store saved before the first result has baseline 0, which restore
// takes as a known per-model baseline. Adopting a mark makes it unknown
// again, so the first reattached turn does not report the CLI's whole
// per-model cumulative as its own.
func TestRestore_AdoptedMarkWithholdsTheFirstTurnsModels(t *testing.T) {
	opus := func(cost float64) clievent.SendResult {
		return clievent.SendResult{CostUSD: cost, ModelUsage: map[string]clievent.ModelUsage{
			"claude-opus-5-5": {OutputTokens: int64(cost * 1000), CostUSD: cost}}}
	}
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	r, _, proc := startCostRouter(t, storePath)
	r.saveIfDirty()
	if e := loadStore(storePath)[shutdownCostKey]; e == nil || e.LastCumulativeCost != 0 {
		t.Fatalf("saved entry = %+v, want baseline 0", e)
	}
	proc.fn(opus(3))
	r.runs.cost.ledger.Close()

	r2 := newCostRouter(t, storePath)
	s, ok := lookupT(r2, shutdownCostKey)
	if !ok {
		t.Fatal("session not restored")
	}
	next := &hookedTestProcess{TestProcess: &TestProcess{AliveVal: true}}
	s.storeProcess(next)
	bookUnownedResults(s, next)
	next.fn(opus(4))
	tot := s.CostTotals()
	if !approxEq(tot.USD, 4) {
		t.Errorf("restored spend = %v, want 4", tot.USD)
	}
	if mu, ok := tot.Models["claude-opus-5-5"]; ok {
		t.Fatalf("first reattached turn booked models %+v, want them withheld", mu)
	}
}

// Spend booked after the last marked row without a row of its own (a cron
// window's) leaves the store ahead of the mark, so the store's baseline wins.
func TestRestore_StoreAheadOfTheMarkKeepsTheStoreBaseline(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	r, s, proc := startCostRouter(t, storePath)
	proc.fn(clievent.SendResult{CostUSD: 1})
	s.BeginCostWindow()
	proc.fn(clievent.SendResult{CostUSD: 3})
	s.EndCostWindow()
	r.saveIfDirty()
	r.runs.cost.ledger.Close()

	r2 := newCostRouter(t, storePath)
	restored, ok := lookupT(r2, shutdownCostKey)
	if !ok {
		t.Fatal("session not restored")
	}
	if got := loadTotalCost(&restored.lastCumulativeCost); !approxEq(got, 3) {
		t.Fatalf("restored baseline = %v, want the store's 3, not the mark's 1", got)
	}
	if got := loadTotalCost(&restored.costSpent); !approxEq(got, 3) {
		t.Fatalf("restored costSpent = %v, want 3", got)
	}
}

// A key deleted and recreated is a new logical session: the old one's marks
// do not move the new one's baseline.
func TestRestore_MarkOfAnEarlierSessionUnderTheKeyIsIgnored(t *testing.T) {
	storePath := crashAfterSave(t, func(_ *ManagedSession, proc *detachingProc) {
		proc.fn(clievent.SendResult{CostUSD: 3})
	})
	restored := loadStore(storePath)[shutdownCostKey]
	if restored == nil {
		t.Fatal("session not saved")
	}
	r := newCostRouter(t, "")
	r.runs.cost = newCostAccounting(costledger.NewStore(datadir.ForStore(storePath).CostRoot(), costledger.Options{}))
	t.Cleanup(r.runs.cost.ledger.Close)
	marks := r.runs.cost.sessionMarks(time.Now().Add(-time.Hour), time.Now())
	if len(marks) != 1 {
		t.Fatalf("marks = %+v, want the one session's", marks)
	}
	restored.CreatedAt++
	r.ss.Update(func(tx sessTx) {
		r.restoreStore(tx, map[string]*storeEntry{shutdownCostKey: restored}, marks)
	})
	s, _ := lookupT(r, shutdownCostKey)
	if got := loadTotalCost(&s.lastCumulativeCost); !approxEq(got, 1) {
		t.Fatalf("restored baseline = %v, want the store's 1", got)
	}
}

// Rows land after costMu is released, so they can reach the ledger out of
// order; the mark with the most spend is the newest.
func TestSessionMarks_HighestSpendWins(t *testing.T) {
	ledger := costledger.NewStore(t.TempDir(), costledger.Options{})
	c := newCostAccounting(ledger)
	row := func(key string, m costledger.SessionMark) {
		e := costledger.Entry{Source: costledger.SourceSession, Kind: costledger.KindTurn, SessionKey: key,
			Backend: "claude", Unit: costledger.UnitUSD, Amount: 1, Mark: &m}
		if !ledger.Append(e) {
			t.Fatal("append rejected")
		}
	}
	row("a", costledger.SessionMark{Spent: 5, Cum: 5, Born: 7})
	row("a", costledger.SessionMark{Spent: 3, Cum: 3, Born: 7})
	row("a", costledger.SessionMark{Spent: 9, Cum: 9, Born: 8})
	row("b", costledger.SessionMark{Spent: 2, Cum: 2, Born: 7})
	unmarked := costledger.Entry{Source: costledger.SourceSession, Kind: costledger.KindTurn, SessionKey: "c",
		Backend: "claude", Unit: costledger.UnitUSD, Amount: 1}
	ledger.Append(unmarked)
	ledger.Close()

	got := c.sessionMarks(time.Now().Add(-time.Hour), time.Now())
	want := map[costMarkKey]costledger.SessionMark{
		{"a", 7}: {Spent: 5, Cum: 5, Born: 7},
		{"a", 8}: {Spent: 9, Cum: 9, Born: 8},
		{"b", 7}: {Spent: 2, Cum: 2, Born: 7},
	}
	if len(got) != len(want) {
		t.Fatalf("marks = %+v, want %+v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("mark %v = %+v, want %+v", k, got[k], w)
		}
	}
}

// Only a live session's own booking carries a mark: forwarded spend comes
// from another process's cumulative, a reopened window's and a partial's
// from no current reading.
func TestLedgerRows_OnlyOwnBookingsAreMarked(t *testing.T) {
	proc := &TestProcess{AliveVal: true}
	s, ledger := newLedgerSession(t, "feishu:direct:marks:general", proc)
	s.createdAt.Store(42)
	s.costAcct.ledger.Rates().Observe(costledger.ModelDelta{Model: "claude-opus-5-5", CostUSD: 1, Tokens: costledger.Tokens{Output: 1000}})
	s.accountTurnCost(&clievent.SendResult{CostUSD: 1}, "own")
	s.BeginCostWindow()
	s.accountTurnCost(&clievent.SendResult{CostUSD: 1.5}, "windowed")
	s.BeginCostWindow() // reopening books the first window as session rows
	s.EndCostWindow()
	s.bookPartialUsage(clievent.ShadowUsage{Models: []clievent.ShadowModel{{Model: "claude-opus-5-5", Output: 250}}}, "partial")
	fresh := &ManagedSession{key: s.key, costAcct: s.costAcct}
	fresh.createdAt.Store(42)
	linkSuccessor(s, fresh, s.CostTotals())
	s.accountTurnCost(&clievent.SendResult{CostUSD: 2}, "forwarded")

	marked := map[string]*costledger.SessionMark{}
	for _, e := range allEntries(t, ledger) {
		marked[e.RunID] = e.Mark
	}
	if m := marked["own"]; m == nil || *m != (costledger.SessionMark{Spent: 1, Cum: 1, Born: 42}) {
		t.Errorf("own row mark = %+v, want spent 1, cum 1, born 42", m)
	}
	if len(marked) != 4 {
		t.Fatalf("rows by run = %+v, want own, reopened window, partial and forwarded", marked)
	}
	for run, m := range marked {
		if run != "own" && m != nil {
			t.Errorf("row %q carries mark %+v, want none", run, *m)
		}
	}
}

// With the ledger off nothing is scanned and restore keeps the store's
// baseline.
func TestRestore_LedgerDisabledKeepsTheStoreBaseline(t *testing.T) {
	storePath := crashAfterSave(t, func(_ *ManagedSession, proc *detachingProc) {
		proc.fn(clievent.SendResult{CostUSD: 3})
	})
	r := NewRouter(RouterConfig{StorePath: storePath, CostLedger: CostLedgerConfig{Disabled: true},
		Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	t.Cleanup(r.Shutdown)
	s, ok := lookupT(r, shutdownCostKey)
	if !ok {
		t.Fatal("session not restored")
	}
	if got := loadTotalCost(&s.lastCumulativeCost); !approxEq(got, 1) {
		t.Fatalf("restored baseline = %v, want the store's 1", got)
	}
}
