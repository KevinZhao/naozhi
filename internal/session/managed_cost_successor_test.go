package session

import (
	"context"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
)

// lateSpendRouter is a spawn router with a ledger whose rates price
// claude-opus-5-5 output at $1 per 1000 tokens, holding a dead session for
// key that has spent $2.
func lateSpendRouter(t *testing.T, key string, hook func(context.Context, cli.SpawnOptions) (processIface, error)) (*Router, *ManagedSession) {
	t.Helper()
	r := spawnRouter(t, 4, hook)
	ledger := costledger.NewStore(t.TempDir(), costledger.Options{})
	t.Cleanup(ledger.Close)
	r.runs.cost = newCostAccounting(ledger, nil)
	ledger.Rates().Observe(costledger.ModelDelta{Model: "claude-opus-5-5", CostUSD: 1, Tokens: costledger.Tokens{Output: 1000}})
	old := injectSession(r, key, newDeadProc())
	old.costAcct = r.runs.cost
	storeTotalCost(&old.costSpent, 2)
	return r, old
}

// halfDollarPartial is a $0.50 partial at lateSpendRouter's rates.
var halfDollarPartial = clievent.ShadowUsage{Models: []clievent.ShadowModel{{Model: "claude-opus-5-5", Output: 500}}}

// Spend the replaced session books while the respawn runs unlocked — its
// process-end partial, a late result — is in the new session's spend.
func TestRespawn_SpendBookedDuringTheSpawnReachesTheNewSession(t *testing.T) {
	g := newGatedSpawn()
	const key = "feishu:direct:late-window:general"
	r, old := lateSpendRouter(t, key, g.hook)

	res := spawnAsync(r, key)
	waitEntered(t, g) // the snapshot is taken
	old.bookPartialUsage(halfDollarPartial, "end:sid:1")
	old.accountTurnCost(&clievent.SendResult{CostUSD: 1}, "run-1")
	close(g.release)
	got := waitResult(t, res)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if usd := got.s.CostTotals().USD; !approxEq(usd, 3.5) {
		t.Fatalf("new session's spend = %v, want 3.5 (2 carried + 0.5 partial + 1 result)", usd)
	}
}

// Spend booked on the replaced session after the respawn committed is
// forwarded, model drill-down included; the replaced session's own process
// baseline still differences its readings.
func TestRespawn_SpendBookedAfterTheCommitIsForwarded(t *testing.T) {
	const key = "feishu:direct:late-commit:general"
	r, old := lateSpendRouter(t, key, func(context.Context, cli.SpawnOptions) (processIface, error) { return newIdleProc(), nil })
	s, err := spawnIn(r, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	old.bookPartialUsage(halfDollarPartial, "end:sid:1")
	if usd := s.CostTotals().USD; !approxEq(usd, 2.5) {
		t.Fatalf("after a late partial: new session's spend = %v, want 2.5", usd)
	}
	opus := func(cost float64) *clievent.SendResult {
		return &clievent.SendResult{CostUSD: cost, ModelUsage: map[string]clievent.ModelUsage{
			"claude-opus-5-5": {OutputTokens: int64(cost * 1000), CostUSD: cost}}}
	}
	old.accountTurnCost(opus(1), "run-1")
	old.accountTurnCost(opus(1.5), "run-2") // the old process's cumulative: 0.5 new
	tot := s.CostTotals()
	if !approxEq(tot.USD, 4) || !approxEq(tot.Models["claude-opus-5-5"].CostUSD, 1.5) {
		t.Fatalf("after two late results: new session's totals = %+v, want USD 4 and opus 1.5", tot)
	}
	if usd := loadTotalCost(&old.costSpent); usd != 2 {
		t.Fatalf("replaced session's own spend = %v, want it left at 2", usd)
	}
}

// A rename keeps the process, so a reading the old session receives after it
// (a Send that was still in flight) is differenced against the renamed
// session's baseline: booked once, there, and not again by its next turn.
func TestRename_ReadingOnTheOldSessionIsTheNewSessionsToDifference(t *testing.T) {
	r := NewRouter(RouterConfig{Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	t.Cleanup(r.Shutdown)
	ledger := costledger.NewStore(t.TempDir(), costledger.Options{})
	t.Cleanup(ledger.Close)
	r.runs.cost = newCostAccounting(ledger, nil)
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&clievent.SendResult{Text: "a", CostUSD: 1}, &clievent.SendResult{Text: "b", CostUSD: 4})}
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return proc, nil }
	const oldKey, newKey = "dashboard:direct:scratch-2:general", "dashboard:direct:promoted-2:general"
	old, _, err := r.GetOrCreate(context.Background(), oldKey, AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Send(context.Background(), "a", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("rename failed")
	}
	fresh, _ := lookupT(r, newKey)

	old.accountTurnCost(&clievent.SendResult{CostUSD: 3}, "run-stale")
	if usd := fresh.CostTotals().USD; !approxEq(usd, 3) {
		t.Fatalf("renamed session's spend after the stale reading = %v, want 3", usd)
	}
	if _, err := fresh.Send(context.Background(), "b", nil, nil); err != nil {
		t.Fatal(err)
	}
	if usd := fresh.CostTotals().USD; !approxEq(usd, 4) {
		t.Fatalf("renamed session's spend = %v, want 4: each reading once", usd)
	}
	var total float64
	for _, e := range allEntries(t, ledger) {
		total += e.Amount
	}
	if !approxEq(total, 4) {
		t.Fatalf("ledger total = %v, want 4", total)
	}
}

// A renamed session that is then respawned: a reading on the first session
// is differenced by the second, which runs the same process, and its
// increment reaches the third, the live one.
func TestSuccessorChain_ReachesTheLiveSession(t *testing.T) {
	a := &ManagedSession{key: "a"}
	a.lastCumulative = costledger.Cumulative{USD: 1}
	storeTotalCost(&a.costSpent, 1)
	b := &ManagedSession{key: "b"}
	copyCostBaseline(b, a)
	c := &ManagedSession{key: "c"}
	snap := b.CostTotals()
	storeTotalCost(&c.costSpent, snap.USD)
	linkSuccessor(b, c, snap)

	a.accountTurnCost(&clievent.SendResult{CostUSD: 3}, "run-1")
	a.addSpent(0.25, costledger.Increment{})
	if usd := c.CostTotals().USD; !approxEq(usd, 3.25) {
		t.Fatalf("live session's spend = %v, want 3.25", usd)
	}
	if b.lastCumulative.USD != 3 || loadTotalCost(&a.costSpent) != 1 || loadTotalCost(&b.costSpent) != 1 {
		t.Fatalf("b baseline %v, a spend %v, b spend %v: want 3, 1, 1",
			b.lastCumulative.USD, loadTotalCost(&a.costSpent), loadTotalCost(&b.costSpent))
	}
}

// Bookings racing the link each land on the new session exactly once,
// whether before the link (in the catch-up) or after it (forwarded).
func TestLinkSuccessor_ConcurrentBookingsLandOnce(t *testing.T) {
	const n = 400
	for round := range 20 {
		old, fresh := &ManagedSession{key: "old"}, &ManagedSession{key: "new"}
		snap := old.CostTotals()
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Go(func() {
				<-start
				old.addSpent(1, costledger.Increment{Metered: map[costledger.Unit]float64{costledger.UnitCredits: 1}})
				if i == n/2 {
					linkSuccessor(old, fresh, snap)
				}
			})
		}
		close(start)
		wg.Wait()
		tot := fresh.CostTotals()
		if tot.USD != n || tot.Metered[costledger.UnitCredits] != n {
			t.Fatalf("round %d: new session's totals = %+v, want %d USD and credits", round, tot, n)
		}
	}
}
