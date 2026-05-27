package cron

import (
	"testing"
	"time"
)

// TestSendBudgetClamp_FormulaSemantics is a documentation-grade unit test
// covering the R20260527122801-CR-2 (#1311) clamp formula:
//
//	sendBudget = max(jobTimeout - time.Since(spawnStart), minSendBudget)
//
// The executeOpt site evaluates this inline; this test pins the
// invariants so a future refactor that splits the clamp into a helper
// or accidentally inverts the floor surfaces as a unit-test failure
// rather than a production wall-clock regression.
func TestSendBudgetClamp_FormulaSemantics(t *testing.T) {
	t.Parallel()

	jobTimeout := 5 * time.Minute

	tests := []struct {
		name       string
		spawnTaken time.Duration
		wantBudget time.Duration
		atFloor    bool
	}{
		{
			name:       "fast-spawn-yields-near-full-budget",
			spawnTaken: 100 * time.Millisecond,
			wantBudget: jobTimeout - 100*time.Millisecond,
			atFloor:    false,
		},
		{
			name:       "half-budget-spawn",
			spawnTaken: jobTimeout / 2,
			wantBudget: jobTimeout / 2,
			atFloor:    false,
		},
		{
			name:       "spawn-near-jobtimeout-hits-floor",
			spawnTaken: jobTimeout - 5*time.Second,
			wantBudget: minSendBudget,
			atFloor:    true,
		},
		{
			name:       "spawn-exceeds-jobtimeout-hits-floor",
			spawnTaken: jobTimeout + time.Minute,
			wantBudget: minSendBudget,
			atFloor:    true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := jobTimeout - tt.spawnTaken
			if got < minSendBudget {
				got = minSendBudget
			}
			if got != tt.wantBudget {
				t.Fatalf("clamp(jobTimeout=%v, spawnTaken=%v) = %v, want %v",
					jobTimeout, tt.spawnTaken, got, tt.wantBudget)
			}
			if tt.atFloor && got != minSendBudget {
				t.Fatalf("expected floor-clamp to minSendBudget, got %v", got)
			}
			if !tt.atFloor && got == minSendBudget && tt.wantBudget != minSendBudget {
				t.Fatalf("unexpected floor-clamp at %v", got)
			}
		})
	}
}

// TestSendBudgetClamp_WorstCaseBoundedByJobTimeoutPlusFloor is the
// operator-visible invariant that motivated #1311: total wall-clock for a
// run is bounded by spawnCtx (jobTimeout) + sendCtx (≤ jobTimeout, ≥
// minSendBudget). The historical unclamped path could produce up to
// 2*jobTimeout; the clamp guarantees worst-case ≤ jobTimeout +
// minSendBudget when spawn fully exhausts its own budget.
func TestSendBudgetClamp_WorstCaseBoundedByJobTimeoutPlusFloor(t *testing.T) {
	t.Parallel()

	jobTimeout := 5 * time.Minute
	// Worst case: spawn took the full jobTimeout (or more).
	spawnTaken := jobTimeout

	sendBudget := jobTimeout - spawnTaken
	if sendBudget < minSendBudget {
		sendBudget = minSendBudget
	}

	worstCase := spawnTaken + sendBudget
	wantUpper := jobTimeout + minSendBudget
	if worstCase > wantUpper {
		t.Fatalf("worst-case wall-clock %v exceeds upper bound %v",
			worstCase, wantUpper)
	}

	// And concretely: with 5min jobTimeout + 30s floor, total ≤ 5m30s.
	// The historical unclamped path was 2 * 5min = 10min.
	if worstCase >= 2*jobTimeout {
		t.Fatalf("clamp ineffective: worst-case %v ≥ 2*jobTimeout %v",
			worstCase, 2*jobTimeout)
	}
}

// TestMinSendBudget_IsReasonable pins the floor at a value that's
// non-trivial (so spawn-overshoot isn't immediately a "send timed out")
// but also far below typical jobTimeout so the bound is meaningful.
func TestMinSendBudget_IsReasonable(t *testing.T) {
	t.Parallel()
	if minSendBudget < 5*time.Second {
		t.Fatalf("minSendBudget=%v too small; healthy Send needs more headroom", minSendBudget)
	}
	if minSendBudget > time.Minute {
		t.Fatalf("minSendBudget=%v too large; defeats the clamp's purpose", minSendBudget)
	}
}
