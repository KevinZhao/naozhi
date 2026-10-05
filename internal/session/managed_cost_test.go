package session

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
)

// newLedgerSession wires a ManagedSession (no run-history store) to a real
// temp-dir ledger so accountTurnCost's entries can be read back.
func newLedgerSession(t *testing.T, key string, proc *TestProcess) (*ManagedSession, *costledger.Store) {
	t.Helper()
	ledger := costledger.NewStore(t.TempDir(), costledger.Options{})
	t.Cleanup(ledger.Close)
	s := &ManagedSession{key: key, costAcct: newCostAccounting(ledger, nil)}
	s.SetBackend("claude")
	s.setWorkspace("/home/u/work/proj")
	s.storeProcess(proc)
	return s, ledger
}

func scripted(results ...*clievent.SendResult) func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
	i := 0
	return func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
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
		&clievent.SendResult{Text: "a", CostUSD: 0.3}, &clievent.SendResult{Text: "b", CostUSD: 0.5})}
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
	mu := func(cost float64, in int64, basis string) map[string]clievent.ModelUsage {
		return map[string]clievent.ModelUsage{"us.anthropic.claude-fable-5-1[1m]": {
			InputTokens: in, CostUSD: cost, CanonicalModel: "claude-fable-5-1", Provider: "bedrock", CostBasis: basis}}
	}
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&clievent.SendResult{Text: "a", CostUSD: 0.3, ModelUsage: mu(0.3, 100, "list")},
		&clievent.SendResult{Text: "b", CostUSD: 0.5, ModelUsage: mu(0.5, 160, "managed")})}
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
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(&clievent.SendResult{Text: "a", CostUSD: 0.4})}
	s, ledger := newLedgerSession(t, "cron:job1", proc)
	s.costAcct.ownedByRun = func(key string) bool { return key == "cron:job1" }
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
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(&clievent.SendResult{Text: "a", CostUSD: 0.4})}
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
	proc.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
		return &clievent.SendResult{Text: "ok"}, nil
	}
	s, ledger := newLedgerSession(t, "feishu:p2p:kiro", proc)
	s.SetBackend("kiro")
	for _, cum := range []float64{2, 4, 4} {
		proc.MeteringVal = []clievent.MeteringEntry{{Value: cum, Unit: "credit"}}
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
	mu := func(cost float64, in int64) map[string]clievent.ModelUsage {
		return map[string]clievent.ModelUsage{"m": {InputTokens: in, CostUSD: cost, CostBasis: "list"}}
	}
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&clievent.SendResult{Text: "a", CostUSD: 1.2, ModelUsage: mu(1.2, 500)},
		&clievent.SendResult{Text: "b", CostUSD: 1.5, ModelUsage: mu(1.5, 600)})}
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
		&clievent.SendResult{Text: "a", CostUSD: 0.3}, &clievent.SendResult{Text: "b", CostUSD: 0.5})}
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
			s.finishRun(nil, &clievent.SendResult{Text: "x", CostUSD: c,
				ModelUsage: map[string]clievent.ModelUsage{"m": {CostUSD: c, InputTokens: int64(c * 10)}}}, nil)
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
		&clievent.SendResult{Text: leakSample, CostUSD: 0.10},
		&clievent.SendResult{Text: "已执行完成。", CostUSD: 0.15})}
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

// A process that ends with frames after its last result books them as a
// partial entry, one row per model, once the end arrives: a Send failing on
// the death books nothing itself, so the turn is not booked twice. With no
// rate learned yet the rows carry tokens only and no basis.
func TestProcessEnd_BooksShadowTokensOnce(t *testing.T) {
	dead := newEndingProcess(cli.StateReady, &cli.ProcessEnd{Shadow: clievent.ShadowUsage{Models: []clievent.ShadowModel{
		{Model: "m[1m]", Input: 40, Output: 8}, {Model: "", CacheRead: 3}, {Model: "zero"}, {Model: "h", Output: 2},
	}}})
	dead.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
		return nil, clierr.ErrProcessExited
	}
	s, ledger := newLedgerSession(t, "feishu:p2p:dead", dead.TestProcess)
	bookProcessEnd(s, dead, "")
	if _, err := s.Send(context.Background(), "hi", nil, nil); err == nil {
		t.Fatal("expected error")
	}
	dead.Kill()
	dead.Kill() // a second teardown path: still one end
	ents := settledEntries(t, s, ledger)
	if len(ents) != 1 || ents[0].Kind != costledger.KindPartial || ents[0].Amount != 0 || ents[0].Basis != costledger.BasisNone {
		t.Fatalf("partial entry = %+v", ents)
	}
	want := []costledger.ModelDelta{
		{Model: "m", RawModel: "m[1m]", Tokens: costledger.Tokens{Input: 40, Output: 8}},
		{Model: "unknown", Tokens: costledger.Tokens{CacheRead: 3}},
		{Model: "h", RawModel: "h", Tokens: costledger.Tokens{Output: 2}},
	}
	if !reflect.DeepEqual(ents[0].Models, want) {
		t.Fatalf("partial rows = %+v, want %+v", ents[0].Models, want)
	}
}

// A partial turn is priced at the rates the CLI's own results taught the
// ledger: the canonical model of a result row matches the raw model id an
// assistant frame names. The amount lands on the entry, its rows and the
// session's spend; a model never priced stays tokens-only and does not mark
// the entry as unknown-priced.
func TestProcessEnd_PricedAtLearnedRates(t *testing.T) {
	const in, out, cr, cw = 4e-6, 20e-6, 0.2e-6, 5e-6
	turn := costledger.Tokens{Input: 6, Output: 400, CacheRead: 90_000, CacheWrite: 3000}
	turnUSD := in*6 + out*400 + cr*90_000 + cw*3000
	proc := newEndingProcess(cli.StateReady, &cli.ProcessEnd{Shadow: clievent.ShadowUsage{Models: []clievent.ShadowModel{
		{Model: "claude-never-priced", Output: 50},
		{Model: "claude-opus-5-5", Input: 3, Output: 1200, CacheRead: 45_000, CacheWrite: 6000},
	}}})
	proc.SendFunc = scripted(&clievent.SendResult{Text: "a", CostUSD: turnUSD, ModelUsage: map[string]clievent.ModelUsage{
		"global.anthropic.claude-opus-5-5[1m]": {
			InputTokens: turn.Input, OutputTokens: turn.Output, CacheReadInputTokens: turn.CacheRead,
			CacheCreationInputTokens: turn.CacheWrite, CostUSD: turnUSD, CanonicalModel: "claude-opus-5-5", CostBasis: "list"},
	}})
	s, ledger := newLedgerSession(t, "feishu:p2p:killed", proc.TestProcess)
	bookProcessEnd(s, proc, "")
	if _, err := s.Send(context.Background(), "priced", nil, nil); err != nil {
		t.Fatal(err)
	}
	proc.Kill()

	// One observation: the learned rate scales the observed cost by the
	// fallback weights, which the partial's own mix then reuses.
	partialTok := costledger.Tokens{Input: 3, Output: 1200, CacheRead: 45_000, CacheWrite: 6000}
	want := turnUSD / weighted(turn) * weighted(partialTok)
	ents := settledEntries(t, s, ledger)
	var p *costledger.Entry
	for i := range ents {
		if ents[i].Kind == costledger.KindPartial {
			p = &ents[i]
		}
	}
	if p == nil || len(p.Models) != 2 {
		t.Fatalf("partial entry = %+v", ents)
	}
	if !approxEq(p.Amount, want) || !approxEq(p.Models[1].CostUSD, want) || p.Models[1].Basis != costledger.BasisList {
		t.Fatalf("priced row = %+v amount %v, want %v at basis list", p.Models[1], p.Amount, want)
	}
	if p.Models[0].CostUSD != 0 || p.Models[0].Basis != costledger.BasisNone || p.Basis != costledger.BasisList {
		t.Fatalf("unpriced row = %+v, entry basis %q: want a tokens-only row with no basis, entry list", p.Models[0], p.Basis)
	}
	if got := s.CostTotals().USD; !approxEq(got, turnUSD+want) {
		t.Fatalf("session spend = %v, want the turn plus the partial estimate %v", got, turnUSD+want)
	}
}

// weighted is the fallback weighting a single observation prices by.
func weighted(t costledger.Tokens) float64 {
	return float64(t.Input) + 5*float64(t.Output) + 0.1*float64(t.CacheRead) + 1.25*float64(t.CacheWrite)
}

// settledEntries waits for s's process-end bookings, then reads the ledger.
func settledEntries(t *testing.T, s *ManagedSession, l *costledger.Store) []costledger.Entry {
	t.Helper()
	if !s.costAcct.waitEnds(5 * time.Second) {
		t.Fatal("process-end booking did not finish")
	}
	return allEntries(t, l)
}

// Rates learned from rows the CLI reported without a costBasis price a
// partial with no basis either; the entry is promoted to list, as a priced
// turn entry is, while its rows keep what the observations reported.
func TestBookPartialUsage_UnreportedBasisPromotedToList(t *testing.T) {
	s, ledger := newLedgerSession(t, "feishu:p2p:nobasis", &TestProcess{AliveVal: true})
	ledger.Rates().Observe(costledger.ModelDelta{Model: "claude-opus-5-5", CostUSD: 0.01, Tokens: costledger.Tokens{Output: 1000}})
	s.bookPartialUsage(clievent.ShadowUsage{Models: []clievent.ShadowModel{{Model: "claude-opus-5-5", Output: 100}}}, "run-1")
	ents := allEntries(t, ledger)
	if len(ents) != 1 || !approxEq(ents[0].Amount, 0.001) || ents[0].Basis != costledger.BasisList ||
		len(ents[0].Models) != 1 || ents[0].Models[0].Basis != costledger.BasisNone {
		t.Fatalf("partial entry = %+v, want 0.001 at entry basis list over a row with no basis", ents)
	}
}

// Raw ids that differ only in a context suffix are one model at one rate, so
// a partial books them as one row carrying both rows' tokens.
func TestBookPartialUsage_OneRowPerCanonicalModel(t *testing.T) {
	s, ledger := newLedgerSession(t, "feishu:p2p:suffix", &TestProcess{AliveVal: true})
	ledger.Rates().Observe(costledger.ModelDelta{Model: "claude-opus-5-5", CostUSD: 0.01, Tokens: costledger.Tokens{Output: 1000}})
	s.bookPartialUsage(clievent.ShadowUsage{Models: []clievent.ShadowModel{
		{Model: "claude-opus-5-5", Input: 11, Output: 100, CacheRead: 7},
		{Model: "claude-sonnet-5-5", Output: 20},
		{Model: "claude-opus-5-5[1m]", Input: 13, Output: 300, CacheWrite: 5},
	}}, "run-1")
	ents := allEntries(t, ledger)
	if len(ents) != 1 || len(ents[0].Models) != 2 {
		t.Fatalf("partial entry = %+v, want one entry with an opus and a sonnet row", ents)
	}
	opus := ents[0].Models[0]
	want := costledger.Tokens{Input: 24, Output: 400, CacheRead: 7, CacheWrite: 5}
	if opus.Model != "claude-opus-5-5" || opus.RawModel != "claude-opus-5-5" || opus.Tokens != want {
		t.Fatalf("opus row = %+v, want both raw ids' tokens %+v under the first raw id", opus, want)
	}
	if !approxEq(opus.CostUSD, ents[0].Amount) || ents[0].Models[1].Model != "claude-sonnet-5-5" {
		t.Fatalf("rows = %+v amount %v: the priced opus row should carry the whole amount", ents[0].Models, ents[0].Amount)
	}
}

// A session with no real directory books its partial under the same empty
// workspace its turn rows carry, not under filepath.Base's ".".
func TestBookPartialUsage_WorkspaceMatchesTurnRows(t *testing.T) {
	s, ledger := newLedgerSession(t, "feishu:p2p:nows", &TestProcess{AliveVal: true})
	s.setWorkspace("")
	turn := s.ledgerEntries(costledger.Increment{USD: 0.01}, "run-0")
	s.bookPartialUsage(clievent.ShadowUsage{Models: []clievent.ShadowModel{{Model: "claude-opus-5-5", Output: 100}}}, "run-1")
	ents := allEntries(t, ledger)
	if len(ents) != 1 || len(turn) != 1 || ents[0].Workspace != "" || turn[0].Workspace != "" {
		t.Fatalf("partial %+v, turn %+v: want both under workspace \"\"", ents, turn)
	}
}

// TestAccountTurnCost_AdoptedBaselineChargesNothingForHistory covers the shim
// adopted with no store entry (adoptLiveShim): its CLI has already spent
// an unknown amount, and the first result it reports is a CUMULATIVE figure
// covering turns this process never saw. Without the flag that figure is
// differenced against zero, so whichever run arrives first after the restart is
// billed for the whole session's history.
//
// The contrast with RestoredBaselineWithholdsModelsOnce is the point: there the
// USD baseline was persisted, so only the per-model rows are withheld and the
// USD delta is real. Here nothing was persisted, so the first report becomes the
// baseline and the increment is zero.
func TestAccountTurnCost_AdoptedBaselineChargesNothingForHistory(t *testing.T) {
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&clievent.SendResult{Text: "a", CostUSD: 12.5},
		&clievent.SendResult{Text: "b", CostUSD: 12.9})}
	s, ledger := newLedgerSession(t, "cron:adopted", proc)
	s.markCostBaselineUnknown()

	for i := 0; i < 2; i++ {
		if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	// 12.5 was already spent before adoption; only the 0.4 this process observed
	// is ours.
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 0.4) {
		t.Fatalf("costSpent = %v, want 0.4 — the pre-adoption 12.5 must not be attributed", got)
	}
	// The baseline-establishing turn writes no ledger row, because ledgerEntries
	// already skips zero increments — the same treatment any other zero-cost turn
	// gets. So the ledger holds exactly the 0.4 this process actually observed,
	// and never a 12.5 row attributing someone else's history to this run.
	ents := allEntries(t, ledger)
	if len(ents) != 1 {
		t.Fatalf("entries = %d, want 1 (the baseline turn is a zero increment)", len(ents))
	}
	if !approxEq(ents[0].Amount, 0.4) {
		t.Errorf("ledger amount = %v, want 0.4", ents[0].Amount)
	}
}

// TestAccountTurnCost_NewSessionStillChargesItsFirstTurn is the control: the
// flag must not leak into the ordinary path, where a first cumulative report
// genuinely IS the increment. Without this, "charge nothing for the first turn"
// would look correct in the test above while silently zeroing every new
// session's opening turn.
func TestAccountTurnCost_NewSessionStillChargesItsFirstTurn(t *testing.T) {
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&clievent.SendResult{Text: "a", CostUSD: 0.7})}
	s := &ManagedSession{key: "feishu:p2p:brandnew"}
	s.storeProcess(proc)
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 0.7) {
		t.Fatalf("costSpent = %v, want 0.7 — a new session's first report is its increment", got)
	}
}

// unownedHook is a process offering only the unowned-result hook.
type unownedHook struct{ fn func(clievent.SendResult) }

func (h *unownedHook) SetOnUnownedResult(fn func(clievent.SendResult)) { h.fn = fn }

// #3096: a turn the CLI starts itself is booked when its result arrives, and
// the next owned turn then charges only its own increment — the cumulative
// differencing keeps the two from counting the same spend twice.
func TestBookUnownedResults_BooksCLIStartedTurnsOnce(t *testing.T) {
	proc := &TestProcess{AliveVal: true, SendFunc: scripted(
		&clievent.SendResult{Text: "owned", CostUSD: 3.5})}
	s, ledger := newLedgerSession(t, "dashboard:direct:host:general", proc)
	hooked := &hookedTestProcess{TestProcess: proc}
	s.storeProcess(hooked)
	hook := &hooked.unownedHook
	bookUnownedResults(s, hooked)
	if hook.fn == nil {
		t.Fatal("bookUnownedResults did not bind the hook")
	}

	hook.fn(clievent.SendResult{CostUSD: 1.25}) // a background-task notification turn
	hook.fn(clievent.SendResult{CostUSD: 3.0})  // another one
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 3.0) {
		t.Fatalf("costSpent after two CLI-started turns = %v, want 3.0", got)
	}
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 3.5) {
		t.Fatalf("costSpent after the owned turn = %v, want 3.5 (only its 0.5 is new)", got)
	}
	var amounts []float64
	for _, e := range allEntries(t, ledger) {
		amounts = append(amounts, e.Amount)
	}
	if len(amounts) != 3 || !approxEq(amounts[0]+amounts[1]+amounts[2], 3.5) {
		t.Fatalf("ledger amounts = %v, want three entries summing to 3.5", amounts)
	}
}

// No run record shares an unowned result's run id, so the id names the CLI
// session reconcile attributes it to: the result's own, else the session's.
func TestBookUnownedResults_RunIDNamesTheCLISession(t *testing.T) {
	proc := &TestProcess{AliveVal: true}
	s, ledger := newLedgerSession(t, "dashboard:direct:host:general", proc)
	hooked := &hookedTestProcess{TestProcess: proc}
	s.storeProcess(hooked)
	bookUnownedResults(s, hooked)

	hooked.fn(clievent.SendResult{CostUSD: 1}) // neither knows the session yet
	hooked.fn(clievent.SendResult{CostUSD: 3, SessionID: "sid-result"})
	s.setSessionID("sid-held")
	hooked.fn(clievent.SendResult{CostUSD: 6})
	hooked.fn(clievent.SendResult{CostUSD: 10, SessionID: "sid-result2"}) // the CLI moved on first
	want := map[float64]string{1: "", 2: "unowned:sid-result:", 3: "unowned:sid-held:", 4: "unowned:sid-result2:"}
	ents := allEntries(t, ledger)
	if len(ents) != len(want) {
		t.Fatalf("entries = %+v, want %d", ents, len(want))
	}
	for _, e := range ents {
		prefix, ok := want[e.Amount]
		id, found := strings.CutPrefix(e.RunID, prefix)
		if !ok || !found || len(id) != 16 || strings.Contains(id, ":") {
			t.Errorf("entry $%v run id = %q, want %q + a bare run id", e.Amount, e.RunID, prefix)
		}
	}
}

// The production process must offer the hook, or bookUnownedResults is a
// silent no-op and the CLI-started turns go unbooked again.
var _ unownedResultNotifier = (*cli.Process)(nil)

type hookedTestProcess struct {
	*TestProcess
	unownedHook
}

// A spawned session binds the hook to itself, so its CLI-started turns land
// on its own ledger.
func TestSpawn_BindsUnownedResultBooking(t *testing.T) {
	r := NewRouter(RouterConfig{Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	t.Cleanup(r.Shutdown)
	proc := &hookedTestProcess{TestProcess: &TestProcess{AliveVal: true}}
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return proc, nil }

	s, _, err := r.GetOrCreate(context.Background(), "dashboard:direct:hooked:general", AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if proc.fn == nil {
		t.Fatal("spawn did not bind the unowned-result hook")
	}
	proc.fn(clievent.SendResult{CostUSD: 2})
	if got := loadTotalCost(&s.costSpent); !approxEq(got, 2) {
		t.Fatalf("costSpent = %v, want 2 booked on the spawned session", got)
	}
}

// A rename moves the process to a fresh session. The hook bound to the old
// session must stop booking (or its readings would be charged under the old
// key AND again by the fresh session, whose baseline did not see them), and
// the fresh session must book the CLI-started turns from then on.
func TestBookUnownedResults_RenameMovesBookingWithTheProcess(t *testing.T) {
	r := NewRouter(RouterConfig{Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	t.Cleanup(r.Shutdown)
	ledger := costledger.NewStore(t.TempDir(), costledger.Options{})
	t.Cleanup(ledger.Close)
	r.runs.cost = newCostAccounting(ledger, nil)
	proc := &hookedTestProcess{TestProcess: &TestProcess{AliveVal: true, SendFunc: scripted(
		&clievent.SendResult{Text: "owned", CostUSD: 4})}}
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return proc, nil }
	const oldKey, newKey = "dashboard:direct:scratch-1:general", "dashboard:direct:promoted:general"
	if _, _, err := r.GetOrCreate(context.Background(), oldKey, AgentOpts{}); err != nil {
		t.Fatal(err)
	}
	proc.fn(clievent.SendResult{CostUSD: 1}) // booked on the old session, before the rename
	oldHook := proc.fn

	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("rename failed")
	}
	oldHook(clievent.SendResult{CostUSD: 2.5}) // a stale call on the old session's hook
	proc.fn(clievent.SendResult{CostUSD: 2.5}) // the same reading through the rebound hook
	fresh, ok := lookupT(r, newKey)
	if !ok {
		t.Fatal("renamed session missing")
	}
	// Booked now, not only when a later owned result catches up — the process
	// may die before one arrives.
	if got := loadTotalCost(&fresh.costSpent); !approxEq(got, 2.5) {
		t.Fatalf("fresh costSpent after the rebound hook = %v, want 2.5 (1 carried + 1.5)", got)
	}
	if _, err := fresh.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}

	if got := loadTotalCost(&fresh.costSpent); !approxEq(got, 4) {
		t.Fatalf("fresh costSpent = %v, want 4 (1 + 1.5 + 1.5, each reading once)", got)
	}
	var total float64
	for _, e := range allEntries(t, ledger) {
		total += e.Amount
		if e.SessionKey == oldKey && e.Amount != 1 {
			t.Errorf("entry %+v booked under the old key after the rename", e)
		}
	}
	if !approxEq(total, 4) {
		t.Fatalf("ledger total = %v, want 4", total)
	}
}
