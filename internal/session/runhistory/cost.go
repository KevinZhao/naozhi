package runhistory

// TurnCostDelta computes one turn's cost from the CLI's cumulative
// `total_cost_usd` (a running total per process incarnation, NOT a per-turn
// increment) and the previous baseline. It charges only positive growth:
// raw > prevCumulative → delta = raw - prevCumulative, baseline = raw;
// otherwise delta = 0 and the baseline is unchanged. The second case covers
// noise turns AND out-of-order result events from concurrent passthrough
// turns: a higher cumulative landing first must not make the earlier one
// look like a reset. INVARIANT: the CLI's per-incarnation reset is handled
// at the session boundary (installFreshSessionLocked zeroes the baseline), so
// raw < prevCumulative here can only be reordering, never a genuine reset.
func TurnCostDelta(raw, prevCumulative float64) (delta, nextCumulative float64) {
	if raw <= prevCumulative {
		// No new spend, or an out-of-order earlier turn already subsumed by the
		// higher baseline; keep the baseline monotonic.
		return 0, prevCumulative
	}
	return raw - prevCumulative, raw
}
