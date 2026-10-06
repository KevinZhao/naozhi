package cron

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

// deltaWindow is a CostWindow for fakes whose spend only grows: the run is
// charged the growth between Begin and End.
type deltaWindow struct{ before costledger.Totals }

func (d *deltaWindow) begin(now costledger.Totals) { d.before = now }

func (d *deltaWindow) end(now costledger.Totals) costledger.Increment { return now.Sub(d.before) }

// costSession is a persistent-mode stand-in: the CLI's cumulative reading
// grows across runs on the same process, and spend mirrors what the session
// layer would have accrued so far.
type costSession struct {
	deltaWindow
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
func (s *costSession) BeginCostWindow()                      { s.begin(s.spend()) }
func (s *costSession) EndCostWindow() costledger.Increment   { return s.end(s.spend()) }
func (s *costSession) spend() costledger.Totals {
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
	s.putJobForTest(j)

	s.executeOpt(j.ID, true)
	s.executeOpt(j.ID, true)

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
type failingCostSession struct {
	deltaWindow
	spent float64
}

func (s *failingCostSession) Send(context.Context, string) (SendResult, error) {
	s.spent = 0.7
	return SendResult{}, errors.New("cli exploded")
}
func (s *failingCostSession) SessionID() string                     { return "" }
func (s *failingCostSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }
func (s *failingCostSession) BeginCostWindow()                      { s.begin(s.spend()) }
func (s *failingCostSession) EndCostWindow() costledger.Increment   { return s.end(s.spend()) }
func (s *failingCostSession) spend() costledger.Totals              { return costledger.Totals{USD: s.spent} }

type failingCostRouter struct{ sess *failingCostSession }

func (r failingCostRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (r failingCostRouter) Reset(string)                                               {}
func (r failingCostRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionExisting, nil
}

func TestLocalRun_SendErrorStillBooksSpend(t *testing.T) {
	s, ledger := newCostScheduler(t, failingCostRouter{sess: &failingCostSession{}})
	j := &Job{ID: "00000000deadbeef", Schedule: "@every 5m", Prompt: "ping"}
	s.putJobForTest(j)
	s.executeOpt(j.ID, true)
	runs := s.RecentRuns(j.ID, 1)
	if len(runs) != 1 || runs[0].State != RunStateFailed || !near(runs[0].CostUSD, 0.7) {
		t.Fatalf("runs = %+v", runs)
	}
	ents := ledgerEntries(t, ledger)
	if len(ents) != 1 || !near(ents[0].Amount, 0.7) || ents[0].Source != costledger.SourceCronLocal {
		t.Fatalf("entries = %+v", ents)
	}
}

// totalsOnlySession spends on every Send and exposes its running total, but
// has no CostWindow.
type totalsOnlySession struct{ spent float64 }

func (s *totalsOnlySession) Send(context.Context, string) (SendResult, error) {
	s.spent += 0.4
	return SendResult{Text: "done", SessionID: "sess-t"}, nil
}
func (s *totalsOnlySession) SessionID() string                     { return "sess-t" }
func (s *totalsOnlySession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }
func (s *totalsOnlySession) CostTotals() costledger.Totals         { return costledger.Totals{USD: s.spent} }

// The cost window is the only way a local run is charged: a session without
// it books zero, even one whose CostTotals grew across the Send.
func TestLocalRun_SessionWithoutCostWindowRecordsZero(t *testing.T) {
	for name, router := range map[string]SessionRouter{
		"no cost capability": okRouter{sid: "sess-1"},
		"CostTotals only":    backendRouter{sess: &totalsOnlySession{}},
	} {
		t.Run(name, func(t *testing.T) {
			s, ledger := newCostScheduler(t, router)
			j := &Job{ID: "fedcba9876543210", Schedule: "@every 5m", Prompt: "ping"}
			s.putJobForTest(j)
			s.executeOpt(j.ID, true)
			if runs := s.RecentRuns(j.ID, 1); len(runs) != 1 || runs[0].CostUSD != 0 {
				t.Fatalf("runs = %+v", runs)
			}
			if ents := ledgerEntries(t, ledger); len(ents) != 0 {
				t.Fatalf("zero-cost run must not write: %+v", ents)
			}
		})
	}
}

func TestAppendLedger_SandboxReceiptAndMetering(t *testing.T) {
	ledger := costledger.NewStore(filepath.Join(t.TempDir(), "cost"), costledger.Options{})
	s := &Scheduler{ledger: ledger}
	job := &Job{ID: "job-sb", Backend: "claude"}
	s.appendLedger(runCtx{jobID: job.ID, runID: "r1", snap: jobSnapshot{workDir: "/w/alpha", backend: job.Backend}}, runOutcome{sandboxMeta: &SandboxRunMeta{CostUSD: 1.25, Basis: costledger.BasisManaged,
		Models: []costledger.ModelDelta{{Model: "m", CostUSD: 1.25, Tokens: costledger.Tokens{Output: 7}}}}, sandbox: true})
	s.appendLedger(runCtx{jobID: job.ID, runID: "r2"}, runOutcome{sandboxMeta: &SandboxRunMeta{CostUSD: 0}, sandbox: true})
	s.appendLedger(runCtx{jobID: "job-k", runID: "r3", snap: jobSnapshot{backend: "kiro"}}, runOutcome{costInc: costledger.Increment{Metered: map[costledger.Unit]float64{costledger.UnitCredits: 3}}})
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
	s.appendLedger(runCtx{jobID: "j"}, runOutcome{costInc: costledger.Increment{USD: 1}})
}

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

// The ledger records the backend the run used — its snapshot's — not the
// job's current one: an UpdateJob landing mid-run applies to the next run.
func TestAppendLedger_RecordsTheSnapshotBackend(t *testing.T) {
	ledger := costledger.NewStore(filepath.Join(t.TempDir(), "cost"), costledger.Options{})
	s := &Scheduler{ledger: ledger, tbl: newJobTable(nil)}
	s.tbl.jobs["j"] = &Job{ID: "j", Backend: "kiro"}
	s.appendLedger(runCtx{jobID: "j", runID: "r", snap: jobSnapshot{backend: "codex"}},
		runOutcome{costInc: costledger.Increment{Metered: map[costledger.Unit]float64{costledger.UnitCredits: 1}}})
	ents := ledgerEntries(t, ledger)
	if len(ents) != 1 || ents[0].Backend != "codex" {
		t.Fatalf("entries = %+v, want one on the snapshot's backend codex", ents)
	}
}

// backendSession spends USD and credits on each Send. It does not implement
// BackendReporter; reportingBackendSession adds it, reporting backend.
type backendSession struct {
	deltaWindow
	backend string
	spent   float64
}

func (s *backendSession) Send(context.Context, string) (SendResult, error) {
	s.spent++
	return SendResult{Text: "done", SessionID: "sess-b"}, nil
}
func (s *backendSession) SessionID() string                     { return "sess-b" }
func (s *backendSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }
func (s *backendSession) BeginCostWindow()                      { s.begin(s.spend()) }
func (s *backendSession) EndCostWindow() costledger.Increment   { return s.end(s.spend()) }
func (s *backendSession) spend() costledger.Totals {
	return costledger.Totals{USD: s.spent, Metered: map[costledger.Unit]float64{costledger.UnitCredits: 2 * s.spent}}
}

type reportingBackendSession struct{ *backendSession }

func (s reportingBackendSession) Backend() string { return s.backend }

type backendRouter struct{ sess Session }

func (r backendRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (r backendRouter) Reset(string)                                               {}
func (r backendRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionExisting, nil
}

// runLedgerBackends runs job once against sess and returns the Backend of
// every ledger row it booked, checking that both the USD and the credit row
// exist.
func runLedgerBackends(t *testing.T, sess Session, agents map[string]AgentOpts, jobBackend string) []string {
	t.Helper()
	ledger := costledger.NewStore(filepath.Join(t.TempDir(), "cost"), costledger.Options{})
	t.Cleanup(ledger.Close)
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(t.TempDir(), "cron.json"), MaxJobs: 5},
		SchedulerDeps{Router: backendRouter{sess: sess}, Ledger: ledger, Agents: agents})
	j := &Job{ID: "0123456789abcdef", Schedule: "@every 5m", Prompt: "ping", Backend: jobBackend}
	s.putJobForTest(j)
	s.executeOpt(j.ID, true)

	ents := ledgerEntries(t, ledger)
	units := map[costledger.Unit]bool{}
	var backends []string
	for _, e := range ents {
		units[e.Unit] = true
		backends = append(backends, e.Backend)
	}
	if len(ents) != 2 || !units[costledger.UnitUSD] || !units[costledger.UnitCredits] {
		t.Fatalf("ledger rows = %+v, want one USD and one credits row", ents)
	}
	return backends
}

func wantAllBackend(t *testing.T, got []string, want string) {
	t.Helper()
	for _, b := range got {
		if b != want {
			t.Fatalf("ledger backends = %q, want every row on %q", got, want)
		}
	}
}

// A session spawned on the router default (kiro here) books its kiro credits
// under kiro, not under the "claude" the job's empty backend field used to
// imply.
func TestLocalRun_LedgerUsesSessionBackend(t *testing.T) {
	sess := reportingBackendSession{&backendSession{backend: "kiro"}}
	wantAllBackend(t, runLedgerBackends(t, sess, nil, ""), "kiro")
}

// A session that cannot say which backend it runs on falls back to the
// resolved spawn option: here the agent's default backend.
func TestLocalRun_LedgerUsesAgentBackendWhenSessionSilent(t *testing.T) {
	agents := map[string]AgentOpts{"general": {Backend: "kiro"}}
	wantAllBackend(t, runLedgerBackends(t, &backendSession{}, agents, ""), "kiro")
	wantAllBackend(t, runLedgerBackends(t, reportingBackendSession{&backendSession{}}, agents, ""), "kiro")
}

// The job's explicit backend still labels the rows when the session agrees,
// and outranks the agent default when the session is silent. A reused session
// running elsewhere outranks it: the rows follow where the spend happened.
func TestLocalRun_LedgerKeepsJobBackendOverride(t *testing.T) {
	agents := map[string]AgentOpts{"general": {Backend: "kiro"}}
	wantAllBackend(t, runLedgerBackends(t, reportingBackendSession{&backendSession{backend: "codex"}}, agents, "codex"), "codex")
	wantAllBackend(t, runLedgerBackends(t, &backendSession{}, agents, "codex"), "codex")
	wantAllBackend(t, runLedgerBackends(t, reportingBackendSession{&backendSession{backend: "kiro"}}, nil, "codex"), "kiro")
}

func TestEffectiveBackend(t *testing.T) {
	cases := []struct {
		name string
		opts AgentOpts
		sess Session
		want string
	}{
		{"session wins", AgentOpts{Backend: "claude"}, reportingBackendSession{&backendSession{backend: "kiro"}}, "kiro"},
		{"empty report falls back", AgentOpts{Backend: "codex"}, reportingBackendSession{&backendSession{}}, "codex"},
		{"no capability falls back", AgentOpts{Backend: "codex"}, &backendSession{}, "codex"},
		{"nothing known", AgentOpts{}, &backendSession{}, ""},
	}
	for _, c := range cases {
		if got := effectiveBackend(c.opts, c.sess); got != c.want {
			t.Errorf("%s: effectiveBackend = %q, want %q", c.name, got, c.want)
		}
	}
}

// rc.backend outranks the snapshot's, which stays the fallback for sandbox
// runs and pre-spawn finishes.
func TestAppendLedger_SessionBackendOutranksSnapshot(t *testing.T) {
	ledger := costledger.NewStore(filepath.Join(t.TempDir(), "cost"), costledger.Options{})
	s := &Scheduler{ledger: ledger, tbl: newJobTable(nil)}
	s.appendLedger(runCtx{jobID: "j", runID: "r", backend: "kiro", snap: jobSnapshot{backend: "codex"}},
		runOutcome{costInc: costledger.Increment{Metered: map[costledger.Unit]float64{costledger.UnitCredits: 1}}})
	ents := ledgerEntries(t, ledger)
	if len(ents) != 1 || ents[0].Backend != "kiro" {
		t.Fatalf("entries = %+v, want one on the session's backend kiro", ents)
	}
}
