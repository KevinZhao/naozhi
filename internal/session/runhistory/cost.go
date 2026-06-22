package runhistory

// TurnCostDelta computes the genuine cost of a single turn from the CLI's
// cumulative `total_cost_usd` reading, given the previous cumulative baseline
// observed for the session.
//
// Why this is needed: Claude CLI reports total_cost_usd as a running total
// for the *current process incarnation*, NOT a per-turn increment. Empirically
// (verified against ~/.naozhi/session-runs) the counter RESETS to a fresh
// running total whenever the process is replaced — i.e. on every resume /
// restart / reset-and-recreate — even when the CLI session_id is reused.
// Storing the raw cumulative value on each per-run record (the old behaviour)
// therefore made a short turn appear to "cost" the entire session-to-date
// total, and summing those raw values over-counted wildly.
//
// The rule keeps the baseline MONOTONIC and charges only the positive growth:
//
//   - growth: raw > prevCumulative → delta = raw - prevCumulative, baseline = raw
//   - no growth: raw <= prevCumulative → delta = 0, baseline unchanged. This
//     single case covers both a noise turn (interrupt / pure-tool / error with
//     raw <= 0) AND a turn whose result event arrives OUT OF ORDER relative to
//     another concurrent turn on the same process: under passthrough, two
//     same-session turns complete on separate goroutines, so finishRun may run
//     them in either order. A later-emitted (higher) cumulative landing first
//     must not make the earlier (lower) one look like a reset — charging it
//     again would double-count. Because the cumulative already includes the
//     reordered turn's cost, dropping it to 0 keeps the running total exact.
//
// CRITICAL INVARIANT: the CLI's per-incarnation RESET is NOT handled here. It
// is handled at the session boundary — installFreshSessionLocked resets the
// persisted baseline to 0 when a new process replaces the old one (resume /
// restart), so every call that reaches this function is within ONE CLI
// incarnation whose cumulative only ever grows. That is precisely why a
// raw < prevCumulative reading can only be an out-of-order arrival, never a
// genuine reset, and must therefore yield delta 0 rather than delta = raw.
//
// It returns the per-turn delta plus the (monotonic) cumulative baseline to
// carry forward. Callers persist nextCumulative as the session's running
// baseline.
func TurnCostDelta(raw, prevCumulative float64) (delta, nextCumulative float64) {
	if raw <= prevCumulative {
		// No new spend: noise turn (raw <= 0) or an out-of-order earlier turn
		// whose cost is already subsumed by the higher baseline. Keep the
		// baseline monotonic so a reordered arrival cannot regress it.
		return 0, prevCumulative
	}
	return raw - prevCumulative, raw
}
