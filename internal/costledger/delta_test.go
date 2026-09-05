package costledger

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDelta_ChargesOnlyPositiveGrowth(t *testing.T) {
	prev := Cumulative{USD: 0.30, Models: map[string]ModelUsage{
		"m[1m]": {Tokens: Tokens{Input: 100, Output: 10}, CostUSD: 0.30, Basis: BasisList},
	}}
	raw := Cumulative{USD: 0.50, Models: map[string]ModelUsage{
		"m[1m]": {Tokens: Tokens{Input: 150, Output: 12}, CostUSD: 0.50, Basis: BasisList, Provider: "bedrock"},
	}}
	inc, next := Delta(raw, prev)
	if !approx(inc.USD, 0.20) || !approx(next.USD, 0.50) {
		t.Fatalf("usd inc=%v next=%v", inc.USD, next.USD)
	}
	if len(inc.Models) != 1 || inc.Models[0].Input != 50 || inc.Models[0].Output != 2 || !approx(inc.Models[0].CostUSD, 0.20) {
		t.Fatalf("model inc = %+v", inc.Models)
	}
	if inc.Models[0].Model != "m" || inc.Models[0].RawModel != "m[1m]" || inc.Models[0].Provider != "bedrock" {
		t.Fatalf("model identity = %+v", inc.Models[0])
	}
	if inc.Basis != BasisList {
		t.Fatalf("basis = %q", inc.Basis)
	}
}

func TestDelta_ReorderedLowerResultIsZeroAndKeepsBaseline(t *testing.T) {
	prev := Cumulative{USD: 0.50, Models: map[string]ModelUsage{"m": {Tokens: Tokens{Input: 150}, CostUSD: 0.50}}}
	raw := Cumulative{USD: 0.30, Models: map[string]ModelUsage{"m": {Tokens: Tokens{Input: 100}, CostUSD: 0.30}}}
	inc, next := Delta(raw, prev)
	if inc.USD != 0 || len(inc.Models) != 0 || inc.Basis != BasisNone {
		t.Fatalf("expected no charge, got %+v", inc)
	}
	if next.USD != 0.50 || next.Models["m"].Input != 150 || next.Models["m"].CostUSD != 0.50 {
		t.Fatalf("baseline lowered: %+v", next)
	}
}

func TestDelta_NewModelFullyCharged_MissingModelRetained(t *testing.T) {
	prev := Cumulative{Models: map[string]ModelUsage{"old": {Tokens: Tokens{Input: 5}, CostUSD: 0.1}}}
	raw := Cumulative{Models: map[string]ModelUsage{"new": {Tokens: Tokens{Output: 7}, CostUSD: 0.2, Basis: BasisUnknown}}}
	inc, next := Delta(raw, prev)
	if len(inc.Models) != 1 || inc.Models[0].RawModel != "new" || inc.Models[0].Output != 7 {
		t.Fatalf("inc = %+v", inc.Models)
	}
	if inc.Basis != BasisUnknown {
		t.Fatalf("worst basis should be unknown, got %q", inc.Basis)
	}
	if _, ok := next.Models["old"]; !ok {
		t.Fatal("model missing from raw must stay in baseline")
	}
}

func TestDelta_WorstBasisWins(t *testing.T) {
	raw := Cumulative{Models: map[string]ModelUsage{
		"a": {CostUSD: 1, Basis: BasisList},
		"b": {CostUSD: 1, Basis: BasisManaged},
	}}
	inc, _ := Delta(raw, Cumulative{})
	if inc.Basis != BasisManaged {
		t.Fatalf("basis = %q", inc.Basis)
	}
}

func TestDelta_MeteredUnitsDifferenced(t *testing.T) {
	prev := Cumulative{Metered: map[Unit]float64{UnitCredits: 2}}
	raw := Cumulative{Metered: map[Unit]float64{UnitCredits: 4, UnitTokens: 900}}
	inc, next := Delta(raw, prev)
	if inc.Metered[UnitCredits] != 2 || inc.Metered[UnitTokens] != 900 {
		t.Fatalf("metered inc = %v", inc.Metered)
	}
	if next.Metered[UnitCredits] != 4 || next.Metered[UnitTokens] != 900 {
		t.Fatalf("metered next = %v", next.Metered)
	}
	// Same cumulative again: nothing new (the P2-class double count).
	inc2, _ := Delta(raw, next)
	if len(inc2.Metered) != 0 {
		t.Fatalf("repeat cumulative must charge nothing, got %v", inc2.Metered)
	}
}

func TestDelta_DoesNotAliasInputMaps(t *testing.T) {
	raw := Cumulative{Models: map[string]ModelUsage{"m": {CostUSD: 1}}, Metered: map[Unit]float64{UnitCredits: 1}}
	_, next := Delta(raw, Cumulative{})
	raw.Models["m"] = ModelUsage{CostUSD: 99}
	raw.Metered[UnitCredits] = 99
	if next.Models["m"].CostUSD != 1 || next.Metered[UnitCredits] != 1 {
		t.Fatal("baseline aliases caller's maps")
	}
}

func TestCanonicalOr(t *testing.T) {
	cases := map[[2]string]string{
		{"claude-fable-5-1", "us.anthropic.claude-fable-5-1[1m]"}: "claude-fable-5-1",
		{"", "us.anthropic.claude-fable-5-1[1m]"}:                 "us.anthropic.claude-fable-5-1",
		{"", "plain"}: "plain",
		{"", "[1m]"}:  "[1m]",
	}
	for in, want := range cases {
		if got := canonicalOr(in[0], in[1]); got != want {
			t.Errorf("canonicalOr(%q,%q)=%q want %q", in[0], in[1], got, want)
		}
	}
}

func TestTotals_SubAndAccumulateRoundTrip(t *testing.T) {
	var tot Totals
	inc1 := Increment{USD: 0.3, Metered: map[Unit]float64{UnitCredits: 2},
		Models: []ModelDelta{{Model: "m", RawModel: "m[1m]", CostUSD: 0.3, Tokens: Tokens{Input: 10}}}}
	before := tot.Accumulate(inc1)
	inc2 := Increment{USD: 0.2, Models: []ModelDelta{{Model: "m", RawModel: "m[1m]", CostUSD: 0.2, Tokens: Tokens{Input: 5}}}}
	after := before.Accumulate(inc2)
	d := after.Sub(before)
	if !approx(d.USD, 0.2) || len(d.Models) != 1 || d.Models[0].Input != 5 || !approx(d.Models[0].CostUSD, 0.2) {
		t.Fatalf("Sub = %+v", d)
	}
	if len(d.Metered) != 0 {
		t.Fatalf("no metered growth expected, got %v", d.Metered)
	}
	if before.USD != 0.3 || before.Models["m[1m]"].Input != 10 {
		t.Fatal("Accumulate mutated its receiver")
	}
	// Second round on top: Sub against the original baseline sees both increments.
	final := after.Accumulate(Increment{USD: 0.1, Metered: map[Unit]float64{UnitCredits: 1}})
	d2 := final.Sub(before)
	if !approx(d2.USD, 0.3) || d2.Metered[UnitCredits] != 1 || d2.Models[0].Input != 5 {
		t.Fatalf("two-round Sub = %+v", d2)
	}
}
