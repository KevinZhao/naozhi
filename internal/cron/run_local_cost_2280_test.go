package cron

import "testing"

// R202606e-ARCH-1 (#2280): local (non-sandbox) cron runs persist their cost
// on CronRun.CostUSD and summary() must surface it. Previously summary() only
// set CostUSD when SandboxMeta != nil, so the most common (local) cron path
// reported cost_usd:0 and the per-job monthly aggregate silently undercounted.
func TestSummary_LocalRunCostFallsBackToRunCostUSD(t *testing.T) {
	r := &CronRun{
		RunID:   "r1",
		JobID:   "j1",
		State:   RunStateSucceeded,
		CostUSD: 0.0421, // local run cost, no SandboxMeta
	}
	s := r.summary()
	if s.CostUSD != 0.0421 {
		t.Fatalf("local run summary CostUSD = %v, want 0.0421", s.CostUSD)
	}
}

// When a sandbox receipt is present its cost wins, and the run-level CostUSD
// (left 0 on the sandbox path) does not override it.
func TestSummary_SandboxCostPreferredOverRunCostUSD(t *testing.T) {
	r := &CronRun{
		RunID:       "r2",
		JobID:       "j1",
		State:       RunStateSucceeded,
		CostUSD:     0, // sandbox path leaves the run-level field 0
		SandboxMeta: &SandboxRunMeta{CostUSD: 1.23},
	}
	s := r.summary()
	if s.CostUSD != 1.23 {
		t.Fatalf("sandbox summary CostUSD = %v, want 1.23 (SandboxMeta wins)", s.CostUSD)
	}
}

// A zero-cost local run stays at 0 (omitempty drops the wire key) — no panic,
// no phantom cost.
func TestSummary_ZeroLocalCostStaysZero(t *testing.T) {
	r := &CronRun{RunID: "r3", JobID: "j1", State: RunStateSucceeded}
	if got := r.summary().CostUSD; got != 0 {
		t.Fatalf("zero-cost local run summary CostUSD = %v, want 0", got)
	}
}
