package cron

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

// costSession is a persistent-mode stand-in: the CLI's cumulative reading
// grows across runs on the same process, and CostTotals mirrors what the
// session layer would have accrued so far.
type costSession struct {
	cumulative []float64
	calls      int
	spent      float64
}

func (s *costSession) Send(context.Context, string) (SendResult, error) {
	if s.calls < len(s.cumulative) {
		s.spent = s.cumulative[s.calls]
	}
	s.calls++
	return SendResult{Text: "done", SessionID: "sess-p"}, nil
}
func (s *costSession) SessionID() string                     { return "sess-p" }
func (s *costSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }
func (s *costSession) CostTotals() costledger.Totals {
	return costledger.Totals{USD: s.spent, Models: map[string]costledger.ModelUsage{
		"m[1m]": {CostUSD: s.spent, Canonical: "m", Basis: costledger.BasisList}}}
}

type costRouter struct{ sess *costSession }

func (r costRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (r costRouter) Reset(string)                                               {}
func (r costRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionExisting, nil
}

func newCostScheduler(t *testing.T, router SessionRouter) (*Scheduler, *costledger.Store) {
	t.Helper()
	ledger := costledger.NewStore(filepath.Join(t.TempDir(), "cost"), costledger.Options{})
	t.Cleanup(ledger.Close)
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(t.TempDir(), "cron.json"), MaxJobs: 5},
		SchedulerDeps{Router: router, Ledger: ledger})
	return s, ledger
}

func ledgerEntries(t *testing.T, l *costledger.Store) []costledger.Entry {
	t.Helper()
	l.Close()
	ents, err := l.Entries(costledger.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}, 50)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}

// P2 regression (docs/rfc/cost-ledger.md §5.3): a persistent-mode job whose
// process reports cumulative 0.3 then 0.5 must record per-run 0.3 and 0.2.
// Before the RFC the raw cumulative crossed the boundary and the second run
// was booked at 0.5.
func TestLocalRun_CostIsPerRunDeltaNotCumulative(t *testing.T) {
	sess := &costSession{cumulative: []float64{0.3, 0.5}}
	s, ledger := newCostScheduler(t, costRouter{sess: sess})
	j := &Job{ID: "0123456789abcdef", Schedule: "@every 5m", Prompt: "ping", WorkDir: "/home/u/proj"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true)
	s.executeOpt(j, true)

	runs := s.RecentRuns(j.ID, 5)
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	// Same-millisecond runs have no stable order; check the multiset.
	a, b := runs[0].CostUSD, runs[1].CostUSD
	if !((near(a, 0.3) && near(b, 0.2)) || (near(a, 0.2) && near(b, 0.3))) {
		t.Fatalf("per-run cost = [%v, %v], want {0.3, 0.2} (old code booked the cumulative 0.5)", a, b)
	}

	ents := ledgerEntries(t, ledger)
	if len(ents) != 2 {
		t.Fatalf("ledger entries = %d: %+v", len(ents), ents)
	}
	for _, e := range ents {
		if e.Source != costledger.SourceCronLocal || e.Kind != costledger.KindTurn || e.JobID != j.ID ||
			e.RunID == "" || e.Workspace != "proj" || e.Backend != "claude" || len(e.Models) != 1 {
			t.Fatalf("entry = %+v", e)
		}
	}
	if !near(ents[0].Amount+ents[1].Amount, 0.5) {
		t.Fatalf("ledger total = %v", ents[0].Amount+ents[1].Amount)
	}
}

// failingCostSession spends and then fails the turn: the partial spend must
// still reach the ledger through the send-error finishRun path.
type failingCostSession struct{ spent float64 }

func (s *failingCostSession) Send(context.Context, string) (SendResult, error) {
	s.spent = 0.7
	return SendResult{}, errors.New("cli exploded")
}
func (s *failingCostSession) SessionID() string                     { return "" }
func (s *failingCostSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }
func (s *failingCostSession) CostTotals() costledger.Totals         { return costledger.Totals{USD: s.spent} }

type failingCostRouter struct{ sess *failingCostSession }

func (r failingCostRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (r failingCostRouter) Reset(string)                                               {}
func (r failingCostRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionExisting, nil
}

func TestLocalRun_SendErrorStillBooksSpend(t *testing.T) {
	s, ledger := newCostScheduler(t, failingCostRouter{sess: &failingCostSession{}})
	j := &Job{ID: "00000000deadbeef", Schedule: "@every 5m", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	s.executeOpt(j, true)
	runs := s.RecentRuns(j.ID, 1)
	if len(runs) != 1 || runs[0].State != RunStateFailed || !near(runs[0].CostUSD, 0.7) {
		t.Fatalf("runs = %+v", runs)
	}
	ents := ledgerEntries(t, ledger)
	if len(ents) != 1 || !near(ents[0].Amount, 0.7) || ents[0].Source != costledger.SourceCronLocal {
		t.Fatalf("entries = %+v", ents)
	}
}

func TestLocalRun_SessionWithoutCostReporterRecordsZero(t *testing.T) {
	s, ledger := newCostScheduler(t, okRouter{sid: "sess-1"})
	j := &Job{ID: "fedcba9876543210", Schedule: "@every 5m", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	s.executeOpt(j, true)
	if runs := s.RecentRuns(j.ID, 1); len(runs) != 1 || runs[0].CostUSD != 0 {
		t.Fatalf("runs = %+v", runs)
	}
	if ents := ledgerEntries(t, ledger); len(ents) != 0 {
		t.Fatalf("zero-cost run must not write: %+v", ents)
	}
}

func TestAppendLedger_SandboxReceiptAndMetering(t *testing.T) {
	ledger := costledger.NewStore(filepath.Join(t.TempDir(), "cost"), costledger.Options{})
	s := &Scheduler{ledger: ledger}
	job := &Job{ID: "job-sb", Backend: "claude"}
	s.appendLedger(finishArgs{job: job, runID: "r1", workDir: "/w/alpha",
		sandboxMeta: &SandboxRunMeta{CostUSD: 1.25, Basis: costledger.BasisManaged,
			Models: []costledger.ModelDelta{{Model: "m", CostUSD: 1.25, Tokens: costledger.Tokens{Output: 7}}}}, sandbox: true})
	s.appendLedger(finishArgs{job: job, runID: "r2", sandboxMeta: &SandboxRunMeta{CostUSD: 0}, sandbox: true})
	s.appendLedger(finishArgs{job: &Job{ID: "job-k", Backend: "kiro"}, runID: "r3",
		costInc: costledger.Increment{Metered: map[costledger.Unit]float64{costledger.UnitCredits: 3}}})
	ents := ledgerEntries(t, ledger)
	if len(ents) != 2 {
		t.Fatalf("entries = %d: %+v", len(ents), ents)
	}
	var receipt, metered *costledger.Entry
	for i := range ents {
		switch ents[i].Kind {
		case costledger.KindReceipt:
			receipt = &ents[i]
		case costledger.KindMetering:
			metered = &ents[i]
		}
	}
	if receipt == nil || receipt.Source != costledger.SourceCronSandbox || receipt.Amount != 1.25 || receipt.Workspace != "alpha" || receipt.RunID != "r1" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if receipt.Basis != costledger.BasisManaged || len(receipt.Models) != 1 || receipt.Models[0].Output != 7 {
		t.Fatalf("receipt drill-down = %+v", receipt)
	}
	if metered == nil || metered.Unit != costledger.UnitCredits || metered.Amount != 3 || metered.Backend != "kiro" {
		t.Fatalf("metered = %+v", metered)
	}
}

func TestAppendLedger_NilLedgerIsNoop(t *testing.T) {
	s := &Scheduler{}
	s.appendLedger(finishArgs{job: &Job{ID: "j"}, costInc: costledger.Increment{USD: 1}})
}

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }
