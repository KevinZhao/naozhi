package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/costledger"
)

// newLedgerSession wires a ManagedSession (no run-history store) to a real
// temp-dir ledger so accountTurnCost's entries can be read back.
func newLedgerSession(t *testing.T, key string, proc *TestProcess) (*ManagedSession, *costledger.Store) {
	t.Helper()
	ledger := costledger.NewStore(t.TempDir(), costledger.Options{})
	t.Cleanup(ledger.Close)
	s := &ManagedSession{key: key, costAcct: newCostAccounting(ledger)}
	s.SetBackend("claude")
	s.setWorkspace("/home/u/work/proj")
	s.storeProcess(proc)
	return s, ledger
}

func scripted(results ...*cli.SendResult) func(context.Context, string, []cli.Attachment, cli.EventCallback) (*cli.SendResult, error) {
	i := 0
	return func(context.Context, string, []cli.Attachment, cli.EventCallback) (*cli.SendResult, error) {
		r := results[i]
		i++
		return r, nil
	}
}

func allEntries(t *testing.T, l *costledger.Store) []costledger.Entry {
	t.Helper()
	l.Close()
	ents, err := l.Entries(costledger.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}, 100)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}

// P4 regression: cost must accrue even when run-history persistence is off.
// On the pre-RFC code finishRun returned before the delta and this failed.
func TestAccountTurnCost_AccruesWithoutRunStore(t *testing.T) {
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&cli.SendResult{Text: "a", CostUSD: 0.3}, &cli.SendResult{Text: "b", CostUSD: 0.5})}
	s := &ManagedSession{key: "feishu:p2p:nostore"}
	s.storeProcess(proc)
	for i := 0; i < 2; i++ {
		if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 0.5) {
		t.Fatalf("costSpent = %v, want 0.5 (deltas 0.3 + 0.2)", got)
	}
	if got := s.CostTotals().USD; !approxEq(got, 0.5) {
		t.Fatalf("CostTotals().USD = %v", got)
	}
}

func TestAccountTurnCost_WritesOneEntryPerTurnWithModels(t *testing.T) {
	mu := func(cost float64, in int64, basis string) map[string]cli.ModelUsage {
		return map[string]cli.ModelUsage{"us.anthropic.claude-fable-5-1[1m]": {
			InputTokens: in, CostUSD: cost, CanonicalModel: "claude-fable-5-1", Provider: "bedrock", CostBasis: basis}}
	}
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&cli.SendResult{Text: "a", CostUSD: 0.3, ModelUsage: mu(0.3, 100, "list")},
		&cli.SendResult{Text: "b", CostUSD: 0.5, ModelUsage: mu(0.5, 160, "managed")})}
	s, ledger := newLedgerSession(t, "feishu:p2p:u1", proc)
	for i := 0; i < 2; i++ {
		if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	ents := allEntries(t, ledger)
	if len(ents) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(ents), ents)
	}
	// newest first
	second, first := ents[0], ents[1]
	if !approxEq(first.Amount, 0.3) || first.Basis != costledger.BasisList || first.Models[0].Input != 100 {
		t.Fatalf("first = %+v", first)
	}
	if !approxEq(second.Amount, 0.2) || second.Basis != costledger.BasisManaged || second.Models[0].Input != 60 ||
		second.Models[0].Model != "claude-fable-5-1" || second.Models[0].Provider != "bedrock" {
		t.Fatalf("second = %+v", second)
	}
	for _, e := range ents {
		if e.Source != costledger.SourceSession || e.Kind != costledger.KindTurn || e.Unit != costledger.UnitUSD ||
			e.SessionKey != "feishu:p2p:u1" || e.Backend != "claude" || e.Workspace != "proj" || e.RunID == "" {
			t.Fatalf("entry identity = %+v", e)
		}
	}
	tot := s.CostTotals()
	if !approxEq(tot.USD, 0.5) || tot.Models["us.anthropic.claude-fable-5-1[1m]"].Input != 160 {
		t.Fatalf("totals = %+v", tot)
	}
}

func TestAccountTurnCost_CronOwnedTurnSkipsLedgerButAccrues(t *testing.T) {
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(&cli.SendResult{Text: "a", CostUSD: 0.4})}
	s, ledger := newLedgerSession(t, "cron:job1", proc)
	s.costAcct.setRunOwnership(func(key string) bool { return key == "cron:job1" })
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 0.4) {
		t.Fatalf("costSpent = %v", got)
	}
	if ents := allEntries(t, ledger); len(ents) != 0 {
		t.Fatalf("cron-owned turn must not write session entries: %+v", ents)
	}
}

func TestAccountTurnCost_CronKeyWithoutGateWritesAsSession(t *testing.T) {
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(&cli.SendResult{Text: "a", CostUSD: 0.4})}
	s, ledger := newLedgerSession(t, "cron:job1", proc)
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	ents := allEntries(t, ledger)
	if len(ents) != 1 || ents[0].Source != costledger.SourceSession {
		t.Fatalf("ungated cron key must still be accounted: %+v", ents)
	}
}

// kiro: the process view is a running sum, so equal readings must not
// re-charge (the P2 class of bug on another backend).
func TestAccountTurnCost_MeteringIsDifferenced(t *testing.T) {
	proc := &TestProcess{AliveVal: true}
	proc.SendFunc = func(context.Context, string, []cli.Attachment, cli.EventCallback) (*cli.SendResult, error) {
		return &cli.SendResult{Text: "ok"}, nil
	}
	s, ledger := newLedgerSession(t, "feishu:p2p:kiro", proc)
	s.SetBackend("kiro")
	for _, cum := range []float64{2, 4, 4} {
		proc.MeteringVal = []cli.MeteringEntry{{Value: cum, Unit: "credit"}}
		if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	ents := allEntries(t, ledger)
	if len(ents) != 2 {
		t.Fatalf("entries = %d, want 2 (third turn added nothing): %+v", len(ents), ents)
	}
	for _, e := range ents {
		if e.Unit != costledger.UnitCredits || e.Kind != costledger.KindMetering || e.Amount != 2 || e.Backend != "kiro" {
			t.Fatalf("metering entry = %+v", e)
		}
	}
	if s.CostTotals().Metered[costledger.UnitCredits] != 4 {
		t.Fatalf("totals = %+v", s.CostTotals())
	}
}

// A store-restored session knows only its USD baseline: the first turn's
// model rows would be the whole incarnation, so they are withheld once.
func TestAccountTurnCost_RestoredBaselineWithholdsModelsOnce(t *testing.T) {
	mu := func(cost float64, in int64) map[string]cli.ModelUsage {
		return map[string]cli.ModelUsage{"m": {InputTokens: in, CostUSD: cost, CostBasis: "list"}}
	}
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&cli.SendResult{Text: "a", CostUSD: 1.2, ModelUsage: mu(1.2, 500)},
		&cli.SendResult{Text: "b", CostUSD: 1.5, ModelUsage: mu(1.5, 600)})}
	s, ledger := newLedgerSession(t, "feishu:p2p:restored", proc)
	storeTotalCost(&s.lastCumulativeCost, 1.0)
	s.lastCumulative = costledger.Cumulative{USD: 1.0}
	s.modelsBaselineUnknown = true
	for i := 0; i < 2; i++ {
		if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	ents := allEntries(t, ledger)
	if len(ents) != 2 {
		t.Fatalf("entries = %d", len(ents))
	}
	second, first := ents[0], ents[1]
	if !approxEq(first.Amount, 0.2) || len(first.Models) != 0 {
		t.Fatalf("first turn after restore = %+v (amount 0.2, no models)", first)
	}
	if !approxEq(second.Amount, 0.3) || len(second.Models) != 1 || second.Models[0].Input != 100 {
		t.Fatalf("second turn = %+v", second)
	}
}

func TestCostTotals_SubAttributesRunWindow(t *testing.T) {
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&cli.SendResult{Text: "a", CostUSD: 0.3}, &cli.SendResult{Text: "b", CostUSD: 0.5})}
	s, _ := newLedgerSession(t, "cron:job1", proc)
	s.Send(context.Background(), "warm", nil, nil)
	before := s.CostTotals()
	s.Send(context.Background(), "run", nil, nil)
	inc := s.CostTotals().Sub(before)
	if !approxEq(inc.USD, 0.2) {
		t.Fatalf("window delta = %v, want 0.2", inc.USD)
	}
}

// Passthrough: same-session turns finish on separate goroutines; costMu must
// make the sum of increments equal the highest cumulative, never more.
func TestAccountTurnCost_ConcurrentTurnsNoLostOrDoubleUpdate(t *testing.T) {
	proc := &TestProcess{AliveVal: true}
	s, ledger := newLedgerSession(t, "feishu:p2p:pt", proc)
	done := make(chan struct{})
	for _, c := range []float64{1, 3, 2, 4} {
		go func(c float64) {
			s.finishRun(nil, nil, &cli.SendResult{Text: "x", CostUSD: c,
				ModelUsage: map[string]cli.ModelUsage{"m": {CostUSD: c, InputTokens: int64(c * 10)}}}, nil)
			done <- struct{}{}
		}(c)
	}
	for range 4 {
		<-done
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 4) {
		t.Fatalf("costSpent = %v, want 4", got)
	}
	var sum float64
	for _, e := range allEntries(t, ledger) {
		sum += e.Amount
	}
	if !approxEq(sum, 4) {
		t.Fatalf("ledger sum = %v, want 4", sum)
	}
	if s.CostTotals().Models["m"].Input != 40 {
		t.Fatalf("model totals = %+v", s.CostTotals().Models)
	}
}

// Leak-recovery runs a second turn on the same process; both rounds are
// accounted and the session total equals the recovered cumulative (#2355).
func TestAccountTurnCost_LeakRecoveryBothRoundsRecorded(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&cli.SendResult{Text: leakSample, CostUSD: 0.10},
		&cli.SendResult{Text: "已执行完成。", CostUSD: 0.15})}
	s, ledger := newLedgerSession(t, "feishu:p2p:leak", proc)
	res, err := s.Send(context.Background(), "go", nil, nil)
	if err != nil || res.Text != "已执行完成。" {
		t.Fatalf("recovery did not fire: %+v err=%v", res, err)
	}
	ents := allEntries(t, ledger)
	if len(ents) != 2 || !approxEq(ents[0].Amount+ents[1].Amount, 0.15) {
		t.Fatalf("entries = %+v", ents)
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 0.15) {
		t.Fatalf("costSpent = %v, want 0.15", got)
	}
}

func TestCopyCostBaseline_RenameKeepsDeltaBaseline(t *testing.T) {
	old := &ManagedSession{key: "a"}
	old.lastCumulative = costledger.Cumulative{USD: 2, Models: map[string]costledger.ModelUsage{"m": {CostUSD: 2}}}
	storeTotalCost(&old.costSpent, 2)
	storeTotalCost(&old.lastCumulativeCost, 2)
	fresh := &ManagedSession{key: "b"}
	copyCostBaseline(fresh, old)
	if fresh.lastCumulative.Models["m"].CostUSD != 2 || loadTotalCost(&fresh.costSpent) != 2 || loadTotalCost(&fresh.lastCumulativeCost) != 2 {
		t.Fatalf("baseline not carried: %+v", fresh.lastCumulative)
	}
	old.lastCumulative.Models["m"] = costledger.ModelUsage{CostUSD: 99}
	if fresh.lastCumulative.Models["m"].CostUSD != 2 {
		t.Fatal("rename must clone the baseline maps, not alias them")
	}
}

// A turn the process died on books its assistant-frame tokens as a partial
// entry (no amount); a failure with the process alive books nothing, since
// the next result's cumulative modelUsage will carry those tokens.
func TestBookPartialTurn_OnProcessDeathOnly(t *testing.T) {
	dead := &TestProcess{AliveVal: true, ShadowVal: cli.ShadowUsage{Model: "m[1m]", Input: 40, Output: 8}}
	dead.SendFunc = func(context.Context, string, []cli.Attachment, cli.EventCallback) (*cli.SendResult, error) {
		return nil, cli.ErrProcessExited
	}
	s, ledger := newLedgerSession(t, "feishu:p2p:dead", dead)
	if _, err := s.Send(context.Background(), "hi", nil, nil); err == nil {
		t.Fatal("expected error")
	}
	ents := allEntries(t, ledger)
	if len(ents) != 1 || ents[0].Kind != costledger.KindPartial || ents[0].Amount != 0 ||
		len(ents[0].Models) != 1 || ents[0].Models[0].Input != 40 || ents[0].Models[0].Model != "m" || ents[0].Models[0].RawModel != "m[1m]" {
		t.Fatalf("partial entry = %+v", ents)
	}

	alive := &TestProcess{AliveVal: true, ShadowVal: cli.ShadowUsage{Input: 40}}
	alive.SendFunc = func(context.Context, string, []cli.Attachment, cli.EventCallback) (*cli.SendResult, error) {
		return nil, errors.New("transient")
	}
	s2, ledger2 := newLedgerSession(t, "feishu:p2p:alive", alive)
	s2.Send(context.Background(), "hi", nil, nil)
	if ents := allEntries(t, ledger2); len(ents) != 0 {
		t.Fatalf("alive failure must not book partial: %+v", ents)
	}
	if alive.ShadowVal.Input != 40 {
		t.Fatal("shadow account must be left for the next result to supersede")
	}
}
