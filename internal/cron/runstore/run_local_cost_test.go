package runstore

import (
	"testing"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// R202606e-ARCH-1 (#2280): cron runs persist their cost on CronRun.CostUSD
// and summary() must surface it, or the per-job monthly aggregate silently
// undercounts.
func TestSummary_LocalRunCostFallsBackToRunCostUSD(t *testing.T) {
	r := &CronRun{
		RunID:   "r1",
		JobID:   "j1",
		State:   runtelemetry.RunStateSucceeded,
		CostUSD: 0.0421,
	}
	s := r.summary()
	if s.CostUSD != 0.0421 {
		t.Fatalf("local run summary CostUSD = %v, want 0.0421", s.CostUSD)
	}
}

// A zero-cost local run stays at 0 (omitempty drops the wire key) — no panic,
// no phantom cost.
func TestSummary_ZeroLocalCostStaysZero(t *testing.T) {
	r := &CronRun{RunID: "r3", JobID: "j1", State: runtelemetry.RunStateSucceeded}
	if got := r.summary().CostUSD; got != 0 {
		t.Fatalf("zero-cost local run summary CostUSD = %v, want 0", got)
	}
}
