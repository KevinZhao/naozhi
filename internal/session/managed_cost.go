package session

import (
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// costAccounting is the router-wide cost sink shared by every ManagedSession:
// the ledger plus the run-ownership gate that hands cron-owned turns to the
// cron scheduler instead of writing them here (docs/rfc/cost-ledger.md §5.0).
type costAccounting struct {
	ledger *costledger.Store

	mu         sync.RWMutex
	ownedByRun func(key string) bool

	warnMu      sync.Mutex
	warnedModel map[string]struct{}
}

// maxWarnedModels bounds the unknown-basis dedup set.
const maxWarnedModels = 64

func newCostAccounting(ledger *costledger.Store) *costAccounting {
	return &costAccounting{ledger: ledger, warnedModel: make(map[string]struct{})}
}

// setRunOwnership installs the gate; nil means no turn is owned elsewhere.
func (c *costAccounting) setRunOwnership(fn func(key string) bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.ownedByRun = fn
	c.mu.Unlock()
}

func (c *costAccounting) owned(key string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	fn := c.ownedByRun
	c.mu.RUnlock()
	return fn != nil && fn(key)
}

// warnUnknownBasis logs once per model whose price the CLI had to guess.
func (c *costAccounting) warnUnknownBasis(key string, models []costledger.ModelDelta) {
	for _, m := range models {
		if m.Basis != costledger.BasisUnknown {
			continue
		}
		c.warnMu.Lock()
		_, seen := c.warnedModel[m.Model]
		if !seen && len(c.warnedModel) < maxWarnedModels {
			c.warnedModel[m.Model] = struct{}{}
		}
		c.warnMu.Unlock()
		if !seen {
			slog.Warn("cost: model priced at costBasis=unknown; CLI guessed the default model's rate",
				"model", osutil.SanitizeForLog(m.Model, 128), "session", osutil.SanitizeForLog(key, 128))
		}
	}
}

// cumulativeFromResult builds the process incarnation's running total from a
// result frame plus the backend metering view (kiro credits / codex tokens,
// both already summed per process by cli.Process).
func cumulativeFromResult(result *cli.SendResult, metering []cli.MeteringEntry) costledger.Cumulative {
	raw := costledger.Cumulative{USD: result.CostUSD}
	if len(result.ModelUsage) > 0 {
		raw.Models = make(map[string]costledger.ModelUsage, len(result.ModelUsage))
		for k, v := range result.ModelUsage {
			raw.Models[k] = costledger.ModelUsage{
				Tokens: costledger.Tokens{
					Input: v.InputTokens, Output: v.OutputTokens,
					CacheRead: v.CacheReadInputTokens, CacheWrite: v.CacheCreationInputTokens,
					Thinking: v.ThinkingTokens, WebSearch: v.WebSearchRequests,
				},
				CostUSD:   v.CostUSD,
				Canonical: v.CanonicalModel,
				Provider:  v.Provider,
				Basis:     costledger.Basis(v.CostBasis),
			}
		}
	}
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
func (s *ManagedSession) accountTurnCost(result *cli.SendResult, runID string) float64 {
	if result == nil {
		return 0
	}
	var metering []cli.MeteringEntry
	if p := s.loadProcess(); p != nil {
		metering = p.MeteringUsage()
	}
	raw := cumulativeFromResult(result, metering)

	s.costMu.Lock()
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

	if s.costAcct != nil && s.costAcct.ledger.Enabled() && !s.costAcct.owned(s.key) {
		s.costAcct.warnUnknownBasis(s.key, inc.Models)
		for _, e := range s.ledgerEntries(inc, runID) {
			s.costAcct.ledger.Append(e)
		}
	}
	return inc.USD
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
