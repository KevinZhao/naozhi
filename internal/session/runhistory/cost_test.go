package runhistory

import "testing"

func TestTurnCostDelta(t *testing.T) {
	const eps = 1e-9
	tests := []struct {
		name           string
		raw            float64
		prev           float64
		wantDelta      float64
		wantCumulative float64
	}{
		{"first turn from zero baseline", 3.0, 0, 3.0, 3.0},
		{"in-process growth", 5.0, 3.0, 2.0, 5.0},
		{"in-process growth again", 8.5, 5.0, 3.5, 8.5},
		// A reading below the baseline is an out-of-order arrival within the
		// same incarnation (the CLI reset is handled at the session boundary,
		// not here): it must NOT be charged again — its cost is already
		// subsumed by the higher baseline. delta 0, baseline stays monotonic.
		{"below baseline is reorder not reset", 2.0, 34.0, 0, 34.0},
		{"below baseline then real growth", 35.0, 34.0, 1.0, 35.0},
		{"noise turn raw zero keeps baseline", 0, 5.0, 0, 5.0},
		{"noise turn negative keeps baseline", -1.0, 5.0, 0, 5.0},
		{"equal to baseline yields zero delta", 5.0, 5.0, 0, 5.0},
		{"first real turn after noise", 6.0, 5.0, 1.0, 6.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			delta, next := TurnCostDelta(tc.raw, tc.prev)
			if delta < tc.wantDelta-eps || delta > tc.wantDelta+eps {
				t.Errorf("delta = %v, want %v", delta, tc.wantDelta)
			}
			if next < tc.wantCumulative-eps || next > tc.wantCumulative+eps {
				t.Errorf("nextCumulative = %v, want %v", next, tc.wantCumulative)
			}
		})
	}
}

// TestTurnCostDelta_SumWithinIncarnation asserts that, within ONE CLI
// incarnation (cumulative monotonically growing, with a noise 0 mid-stream),
// the per-turn deltas sum to the final cumulative — i.e. exactly the money
// the CLI reports, never the over-counted sum-of-snapshots the old per-run
// records produced.
func TestTurnCostDelta_SumWithinIncarnation(t *testing.T) {
	raws := []float64{3.228, 3.313, 3.689, 4.252, 0, 5.359, 10.715}
	const wantTotal = 10.715 // last (highest) cumulative; noise 0 contributes nothing

	var sum, baseline float64
	for _, raw := range raws {
		var d float64
		d, baseline = TurnCostDelta(raw, baseline)
		sum += d
	}
	if sum < wantTotal-1e-6 || sum > wantTotal+1e-6 {
		t.Fatalf("delta sum = %v, want %v", sum, wantTotal)
	}

	// Sanity: naive sum-of-snapshots (the old bug) massively over-counts.
	var naive float64
	for _, raw := range raws {
		naive += raw
	}
	if naive <= wantTotal {
		t.Fatalf("expected naive sum %v to over-count vs true %v", naive, wantTotal)
	}
}

// TestTurnCostDelta_OutOfOrderArrival is the regression guard for the
// concurrent-passthrough hazard: two same-session turns complete on separate
// goroutines, so finishRun may apply a later (higher) cumulative before an
// earlier (lower) one. The total must equal the highest cumulative regardless
// of arrival order — the lower reading is already subsumed and must not be
// charged again.
func TestTurnCostDelta_OutOfOrderArrival(t *testing.T) {
	// In-order: cumulative 2 then 5 → total 5.
	var inOrder, b1 float64
	for _, raw := range []float64{2.0, 5.0} {
		var d float64
		d, b1 = TurnCostDelta(raw, b1)
		inOrder += d
	}
	// Reordered: the higher cumulative (5) lands first, then the lower (2).
	var reordered, b2 float64
	for _, raw := range []float64{5.0, 2.0} {
		var d float64
		d, b2 = TurnCostDelta(raw, b2)
		reordered += d
	}
	if inOrder != 5.0 {
		t.Fatalf("in-order total = %v, want 5.0", inOrder)
	}
	if reordered != inOrder {
		t.Fatalf("reordered total = %v, want %v (order must not affect total)", reordered, inOrder)
	}
	if b1 != b2 || b2 != 5.0 {
		t.Fatalf("baseline must converge to 5.0 regardless of order: b1=%v b2=%v", b1, b2)
	}
}
