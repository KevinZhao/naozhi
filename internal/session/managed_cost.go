package session

import (
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/costledger/cliusage"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// costAccounting is the router-wide cost sink shared by every ManagedSession:
// the ledger plus the run-ownership gate that hands cron-owned turns to the
// cron scheduler instead of writing them here (docs/rfc/cost-ledger.md §5.0).
type costAccounting struct {
	ledger *costledger.Store
	// ownedByRun reports a turn some run writes the ledger for itself; nil
	// means none is.
	ownedByRun func(key string) bool

	warnMu      sync.Mutex
	warnedModel map[string]struct{}

	// ends counts the process-end bookings in flight; endSem bounds how many
	// run at once (managed_cost_end.go).
	ends   inflight
	endSem chan struct{}
}

// maxWarnedModels bounds the unknown-basis dedup set.
const maxWarnedModels = 64

func newCostAccounting(ledger *costledger.Store, ownedByRun func(key string) bool) *costAccounting {
	return &costAccounting{ledger: ledger, ownedByRun: ownedByRun, warnedModel: make(map[string]struct{}),
		endSem: make(chan struct{}, maxEndBookings)}
}

func (c *costAccounting) owned(key string) bool {
	return c != nil && c.ownedByRun != nil && c.ownedByRun(key)
}

// warnUnknownBasis logs once per model whose price the CLI had to guess.
func (c *costAccounting) warnUnknownBasis(key string, models []costledger.ModelDelta) {
	for _, m := range models {
		if m.Basis == costledger.BasisUnknown && c.firstWarn(m.Model) {
			slog.Warn("cost: model priced at costBasis=unknown; CLI guessed the default model's rate",
				"model", osutil.SanitizeForLog(m.Model, 128), "session", osutil.SanitizeForLog(key, 128))
		}
	}
}

// firstWarn reports whether tag is warned about for the first time.
func (c *costAccounting) firstWarn(tag string) bool {
	c.warnMu.Lock()
	defer c.warnMu.Unlock()
	if _, seen := c.warnedModel[tag]; seen {
		return false
	}
	if len(c.warnedModel) < maxWarnedModels {
		c.warnedModel[tag] = struct{}{}
	}
	return true
}

// cumulativeFromResult builds the process incarnation's running total from a
// result frame plus the backend metering view (kiro credits / codex tokens,
// both already summed per process by cli.Process).
func cumulativeFromResult(result *clievent.SendResult, metering []clievent.MeteringEntry) costledger.Cumulative {
	raw := cliusage.Cumulative(result.CostUSD, result.ModelUsage)
	for _, m := range metering {
		u, ok := meteringUnit(m.Unit)
		if !ok {
			continue
		}
		if raw.Metered == nil {
			raw.Metered = make(map[costledger.Unit]float64, 2)
		}
		raw.Metered[u] += m.Value
	}
	return raw
}

// meteringUnit maps backend metering unit labels onto ledger units.
func meteringUnit(u string) (costledger.Unit, bool) {
	switch u {
	case "credit", "credits":
		return costledger.UnitCredits, true
	case "token", "tokens":
		return costledger.UnitTokens, true
	}
	return "", false
}

// accountTurnCost differences the turn's cumulative readings against the
// session baseline, folds the increment into the monotonic totals and, unless
// a cron run owns the turn, appends the ledger entries. It runs on every
// completed turn regardless of run-history persistence. costMu is a leaf
// lock: nothing inside it calls out. Returns the turn's USD increment.
func (s *ManagedSession) accountTurnCost(result *clievent.SendResult, runID string) float64 {
	return s.accountCost(result, runID, nil)
}

// accountCost is accountTurnCost for a reading that counts only while onlyFor
// is still s's process (nil: always). Checked under costMu, so it is atomic
// with RenameSession, which moves the process before copying the baseline: a
// reading lands on the old session before the copy, or is dropped and picked
// up by the new session's next one — never booked on both.
func (s *ManagedSession) accountCost(result *clievent.SendResult, runID string, onlyFor processIface) float64 {
	if result == nil {
		return 0
	}
	var metering []clievent.MeteringEntry
	if p := s.loadProcess(); p != nil {
		metering = p.MeteringUsage()
	}
	raw := cumulativeFromResult(result, metering)

	s.costMu.Lock()
	if onlyFor != nil && s.loadProcess() != onlyFor {
		s.costMu.Unlock()
		return 0
	}
	if s.costBaselineUnknown {
		// Adopt whatever the CLI has counted so far as the baseline, so this turn
		// reports a zero increment and every later turn differences correctly
		// against it. The pre-adoption spend stays unattributed on purpose: this
		// process never saw the turns that produced it, and guessing would put a
		// whole session's history on one run's bill.
		s.costBaselineUnknown = false
		s.lastCumulative = raw
	}
	inc, next := costledger.Delta(raw, s.lastCumulative)
	s.lastCumulative = next
	storeTotalCost(&s.lastCumulativeCost, next.USD)
	if inc.USD != 0 {
		storeTotalCost(&s.costSpent, loadTotalCost(&s.costSpent)+inc.USD)
	}
	dropModels := s.modelsBaselineUnknown
	s.modelsBaselineUnknown = false
	if dropModels {
		inc.Models = nil
	}
	s.spent = s.spent.Accumulate(inc)
	s.costMu.Unlock()

	if s.costAcct != nil {
		rates := s.costAcct.ledger.Rates()
		for _, m := range inc.Models {
			rates.Observe(m)
		}
	}
	if s.costAcct != nil && s.costAcct.ledger.Enabled() && !s.costAcct.owned(s.key) {
		s.costAcct.warnUnknownBasis(s.key, inc.Models)
		for _, e := range s.ledgerEntries(inc, runID) {
			s.costAcct.ledger.Append(e)
		}
	}
	return inc.USD
}

// bookUnownedResults books the cost of the turns proc's CLI starts on its own
// (background-task notifications): their results reach no Send, so without
// this their spend waited for the next owned result's cumulative and was lost
// when the process died first (#3096). The cumulative differencing makes a
// reading booked here and again by a later Send harmless.
func bookUnownedResults(s *ManagedSession, proc processIface) {
	if n, ok := proc.(unownedResultNotifier); ok {
		n.SetOnUnownedResult(func(res clievent.SendResult) { s.accountCost(&res, newRunID(), proc) })
	}
}

// bookPartialUsage records a Kind=partial entry for u, spend a process
// reported in no result frame (bookProcessEnd, which has already applied the
// cron-ownership gate), one row per model priced at the rates the ledger
// learned from the CLI's own results, and adds its amount to the session's
// spend. A model with no learned rate books tokens only and no basis:
// BasisUnknown means the CLI guessed a rate, and here nothing priced it.
func (s *ManagedSession) bookPartialUsage(u clievent.ShadowUsage, runID string) {
	if s.costAcct == nil || !s.costAcct.ledger.Enabled() || u.IsZero() {
		return
	}
	e := costledger.Entry{
		Source: costledger.SourceSession, Kind: costledger.KindPartial,
		SessionKey: s.key, RunID: runID, Workspace: filepath.Base(s.Workspace()), Backend: s.Backend(),
		Unit:   costledger.UnitUSD,
		Models: make([]costledger.ModelDelta, 0, len(u.Models)),
	}
	if e.Backend == "" {
		e.Backend = "claude"
	}
	rates := s.costAcct.ledger.Rates()
	for _, m := range u.Models {
		t := costledger.Tokens{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite}
		if t == (costledger.Tokens{}) {
			continue
		}
		d := costledger.ModelDelta{Model: costledger.CanonicalModel("", m.Model), RawModel: m.Model, Tokens: t}
		if d.Model == "" {
			d.Model = "unknown"
		}
		usd, basis, priced := rates.Estimate(d.Model, t)
		if !priced && s.costAcct.firstWarn("partial:"+d.Model) {
			slog.Warn("cost: no learned rate for a partial turn's model; booked tokens only",
				"model", osutil.SanitizeForLog(d.Model, 128), "session", osutil.SanitizeForLog(s.key, 128))
		}
		d.CostUSD, d.Basis = usd, basis
		e.Amount += usd
		e.Basis = costledger.WorseBasis(e.Basis, basis)
		e.Models = append(e.Models, d)
	}
	if e.Basis == costledger.BasisNone && e.Amount > 0 {
		e.Basis = costledger.BasisList
	}
	if e.Amount > 0 {
		s.costMu.Lock()
		storeTotalCost(&s.costSpent, loadTotalCost(&s.costSpent)+e.Amount)
		s.costMu.Unlock()
	}
	s.costAcct.ledger.Append(e)
}

// ledgerEntries renders an Increment as ledger rows: one USD row carrying the
// model drill-down, plus one metering row per backend unit that grew.
func (s *ManagedSession) ledgerEntries(inc costledger.Increment, runID string) []costledger.Entry {
	base := costledger.Entry{
		Source:     costledger.SourceSession,
		SessionKey: s.key,
		RunID:      runID,
		Workspace:  filepath.Base(s.Workspace()),
		Backend:    s.Backend(),
	}
	if base.Backend == "" {
		base.Backend = "claude"
	}
	if base.Workspace == "." || base.Workspace == string(filepath.Separator) {
		base.Workspace = ""
	}
	var out []costledger.Entry
	if inc.USD > 0 || len(inc.Models) > 0 {
		e := base
		e.Kind, e.Unit, e.Amount, e.Basis, e.Models = costledger.KindTurn, costledger.UnitUSD, inc.USD, inc.Basis, inc.Models
		if e.Basis == costledger.BasisNone && inc.USD > 0 {
			e.Basis = costledger.BasisList
		}
		out = append(out, e)
	}
	for u, v := range inc.Metered {
		e := base
		e.Kind, e.Unit, e.Amount = costledger.KindMetering, u, v
		out = append(out, e)
	}
	return out
}

// CostTotals returns the session's monotonic spend snapshot (USD, backend
// metering units, per-model cumulative deltas). Run owners read it before and
// after a turn and attribute the difference (docs/rfc/cost-ledger.md §5.3).
func (s *ManagedSession) CostTotals() costledger.Totals {
	s.costMu.Lock()
	t := s.spent
	t.USD = loadTotalCost(&s.costSpent)
	if len(s.spent.Metered) > 0 {
		t.Metered = make(map[costledger.Unit]float64, len(s.spent.Metered))
		for k, v := range s.spent.Metered {
			t.Metered[k] = v
		}
	}
	if len(s.spent.Models) > 0 {
		t.Models = make(map[string]costledger.ModelUsage, len(s.spent.Models))
		for k, v := range s.spent.Models {
			t.Models[k] = v
		}
	}
	s.costMu.Unlock()
	return t
}

// copyCostBaseline carries the delta baseline and totals from old to fresh
// when the SAME live process keeps running under a new key (rename). Maps
// are cloned so the two sessions never share mutable state.
func copyCostBaseline(fresh, old *ManagedSession) {
	old.costMu.Lock()
	fresh.lastCumulative = cloneCumulative(old.lastCumulative)
	fresh.spent = old.spent.Accumulate(costledger.Increment{})
	fresh.modelsBaselineUnknown = old.modelsBaselineUnknown
	fresh.costBaselineUnknown = old.costBaselineUnknown
	fresh.endMark = old.endMark
	old.costMu.Unlock()
	storeTotalCost(&fresh.costSpent, loadTotalCost(&old.costSpent))
	storeTotalCost(&fresh.lastCumulativeCost, loadTotalCost(&old.lastCumulativeCost))
}

func cloneCumulative(c costledger.Cumulative) costledger.Cumulative {
	out := costledger.Cumulative{USD: c.USD}
	if len(c.Models) > 0 {
		out.Models = make(map[string]costledger.ModelUsage, len(c.Models))
		for k, v := range c.Models {
			out.Models[k] = v
		}
	}
	if len(c.Metered) > 0 {
		out.Metered = make(map[costledger.Unit]float64, len(c.Metered))
		for k, v := range c.Metered {
			out.Metered[k] = v
		}
	}
	return out
}

// newRunID is the run-history ID generator shared by finishRun and the
// ledger so both records of one turn cross-reference.
func newRunID() string {
	id, err := runhistory.NewRunID()
	if err != nil {
		return ""
	}
	return id
}

// markCostBaselineUnknown declares that this session's CLI has already spent an
// unknown amount that no baseline records. Set by the adopt path only; the next
// accountTurnCost consumes it. See the field comment in managed.go.
func (s *ManagedSession) markCostBaselineUnknown() {
	s.costMu.Lock()
	s.costBaselineUnknown = true
	s.costMu.Unlock()
}
