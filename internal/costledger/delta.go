package costledger

import "strings"

// ModelUsage is one model's cumulative usage inside a CLI process
// incarnation (the ledger-side mirror of the CLI's modelUsage row).
type ModelUsage struct {
	Tokens
	CostUSD   float64
	Canonical string
	Provider  string
	Basis     Basis
}

// Cumulative is a process incarnation's running total as reported by the CLI
// (USD + per-model rows) and by backend metering (kiro credits, codex
// tokens). Every field only grows within one incarnation; Delta charges the
// positive growth and keeps the baseline monotonic.
type Cumulative struct {
	USD     float64
	Models  map[string]ModelUsage // key = raw model string
	Metered map[Unit]float64
}

// Increment is what one turn added on top of the previous baseline.
type Increment struct {
	USD     float64
	Models  []ModelDelta
	Metered map[Unit]float64
	Basis   Basis // worst basis among models that grew; "" when none did
}

// Delta charges only positive growth per field (same rule as
// runhistory.TurnCostDelta): raw <= prev means "no new spend or an
// out-of-order earlier result", so the delta is 0 and the baseline keeps the
// higher value. Models present in prev but absent from raw stay in the
// baseline untouched. The returned baseline never aliases raw's maps.
func Delta(raw, prev Cumulative) (Increment, Cumulative) {
	var inc Increment
	next := Cumulative{USD: prev.USD}
	if raw.USD > prev.USD {
		inc.USD = raw.USD - prev.USD
		next.USD = raw.USD
	}

	if len(prev.Models) > 0 || len(raw.Models) > 0 {
		next.Models = make(map[string]ModelUsage, len(prev.Models)+len(raw.Models))
		for k, v := range prev.Models {
			next.Models[k] = v
		}
		for k, cur := range raw.Models {
			base := prev.Models[k]
			d, nb, grew := modelGrowth(cur, base)
			if grew {
				d.RawModel = k
				d.Model = canonicalOr(cur.Canonical, k)
				d.Provider = cur.Provider
				d.Basis = cur.Basis
				inc.Models = append(inc.Models, d)
				inc.Basis = worseBasis(inc.Basis, cur.Basis)
			}
			nb.Canonical, nb.Provider, nb.Basis = cur.Canonical, cur.Provider, cur.Basis
			next.Models[k] = nb
		}
	}

	if len(prev.Metered) > 0 || len(raw.Metered) > 0 {
		next.Metered = make(map[Unit]float64, len(prev.Metered)+len(raw.Metered))
		for u, v := range prev.Metered {
			next.Metered[u] = v
		}
		for u, cur := range raw.Metered {
			if cur > prev.Metered[u] {
				if inc.Metered == nil {
					inc.Metered = make(map[Unit]float64, len(raw.Metered))
				}
				inc.Metered[u] = cur - prev.Metered[u]
				next.Metered[u] = cur
			}
		}
	}
	return inc, next
}

// modelGrowth differences one model row field by field. The baseline keeps
// the per-field maximum so a reordered lower row can never lower it.
func modelGrowth(cur, base ModelUsage) (d ModelDelta, next ModelUsage, grew bool) {
	pos := func(a, b int64) int64 {
		if a > b {
			return a - b
		}
		return 0
	}
	max64 := func(a, b int64) int64 {
		if a > b {
			return a
		}
		return b
	}
	d.Input = pos(cur.Input, base.Input)
	d.Output = pos(cur.Output, base.Output)
	d.CacheRead = pos(cur.CacheRead, base.CacheRead)
	d.CacheWrite = pos(cur.CacheWrite, base.CacheWrite)
	d.Thinking = pos(cur.Thinking, base.Thinking)
	d.WebSearch = pos(cur.WebSearch, base.WebSearch)
	if cur.CostUSD > base.CostUSD {
		d.CostUSD = cur.CostUSD - base.CostUSD
	}
	next.Input = max64(cur.Input, base.Input)
	next.Output = max64(cur.Output, base.Output)
	next.CacheRead = max64(cur.CacheRead, base.CacheRead)
	next.CacheWrite = max64(cur.CacheWrite, base.CacheWrite)
	next.Thinking = max64(cur.Thinking, base.Thinking)
	next.WebSearch = max64(cur.WebSearch, base.WebSearch)
	next.CostUSD = base.CostUSD
	if cur.CostUSD > base.CostUSD {
		next.CostUSD = cur.CostUSD
	}
	grew = d.CostUSD > 0 || d.Input > 0 || d.Output > 0 || d.CacheRead > 0 ||
		d.CacheWrite > 0 || d.Thinking > 0 || d.WebSearch > 0
	return d, next, grew
}

// CanonicalModel returns the CLI's canonical model id, falling back to the
// raw key with any [1m]/[2m] context suffix stripped.
func CanonicalModel(canonical, raw string) string { return canonicalOr(canonical, raw) }

// canonicalOr returns the CLI's canonical model id, falling back to the raw
// key with any [1m]/[2m] context suffix stripped.
func canonicalOr(canonical, raw string) string {
	if canonical != "" {
		return canonical
	}
	if i := strings.IndexByte(raw, '['); i > 0 && strings.HasSuffix(raw, "]") {
		return raw[:i]
	}
	return raw
}

// Totals is a monotonic, cross-incarnation spend snapshot a run owner reads
// before and after a turn to attribute the difference (docs/rfc §5.3).
type Totals struct {
	USD     float64
	Metered map[Unit]float64
	Models  map[string]ModelUsage // cumulative per-model deltas, keyed by raw model
}

// Sub returns a - b as an Increment, clamping every field at zero.
func (a Totals) Sub(b Totals) Increment {
	var inc Increment
	if a.USD > b.USD {
		inc.USD = a.USD - b.USD
	}
	for u, v := range a.Metered {
		if v > b.Metered[u] {
			if inc.Metered == nil {
				inc.Metered = make(map[Unit]float64, len(a.Metered))
			}
			inc.Metered[u] = v - b.Metered[u]
		}
	}
	for k, cur := range a.Models {
		d, _, grew := modelGrowth(cur, b.Models[k])
		if grew {
			d.RawModel = k
			d.Model = canonicalOr(cur.Canonical, k)
			d.Provider, d.Basis = cur.Provider, cur.Basis
			inc.Models = append(inc.Models, d)
			inc.Basis = worseBasis(inc.Basis, cur.Basis)
		}
	}
	return inc
}

// Accumulate folds an Increment into Totals, returning a new value (the
// receiver's maps are not mutated).
func (a Totals) Accumulate(inc Increment) Totals {
	out := Totals{USD: a.USD + inc.USD}
	if len(a.Metered) > 0 || len(inc.Metered) > 0 {
		out.Metered = make(map[Unit]float64, len(a.Metered)+len(inc.Metered))
		for u, v := range a.Metered {
			out.Metered[u] = v
		}
		for u, v := range inc.Metered {
			out.Metered[u] += v
		}
	}
	if len(a.Models) > 0 || len(inc.Models) > 0 {
		out.Models = make(map[string]ModelUsage, len(a.Models)+len(inc.Models))
		for k, v := range a.Models {
			out.Models[k] = v
		}
		for _, d := range inc.Models {
			m := out.Models[d.RawModel]
			m.Tokens = m.Tokens.add(d.Tokens)
			m.CostUSD += d.CostUSD
			m.Canonical, m.Provider, m.Basis = d.Model, d.Provider, d.Basis
			out.Models[d.RawModel] = m
		}
	}
	return out
}
