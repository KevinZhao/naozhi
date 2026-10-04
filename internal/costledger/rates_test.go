package costledger

import (
	"testing"
	"time"
)

// price is a model's list price per token: input, output, cache read, cache write.
type price [4]float64

func (p price) of(t Tokens) float64 {
	return p[0]*float64(t.Input) + p[1]*float64(t.Output) + p[2]*float64(t.CacheRead) + p[3]*float64(t.CacheWrite)
}

// opus55 is a model whose cache reads cost 0.05x input, not the 0.1x the
// fallback weights assume.
var opus55 = price{4e-6, 20e-6, 0.2e-6, 5e-6}

func varied(i int) Tokens {
	return Tokens{Input: int64(3 + i), Output: int64(200 + 97*i), CacheRead: int64(40000 + 9001*i*i), CacheWrite: int64(1500 + 733*(i%3))}
}

// Enough priced rows recover the model's own per-field prices, so a turn
// with a different token mix is priced as the CLI would have priced it. The
// fallback weights misprice this mix: they put cache reads at 0.1x input.
func TestRateBook_FitsPerFieldPrices(t *testing.T) {
	b := NewRateBook()
	for i := range minFitObservations {
		tok := varied(i)
		b.Observe(ModelDelta{Model: "claude-opus-5-5", RawModel: "claude-opus-5-5[1m]", CostUSD: opus55.of(tok), Basis: BasisList, Tokens: tok})
	}
	mix := Tokens{Input: 10, Output: 9000, CacheRead: 2_000_000, CacheWrite: 120_000}
	got, basis, ok := b.Estimate("claude-opus-5-5", mix)
	if !ok || basis != BasisList {
		t.Fatalf("Estimate ok=%v basis=%q", ok, basis)
	}
	if want := opus55.of(mix); !approx(got, want) {
		t.Fatalf("Estimate = %.6f, want %.6f (the CLI's own price for this mix)", got, want)
	}
}

// Before a fit is possible, the observed cost scales with the fallback
// weights: the observed row itself round-trips exactly.
func TestRateBook_RoundTripsOneObservation(t *testing.T) {
	b := NewRateBook()
	tok := Tokens{Input: 12, Output: 340, CacheRead: 56_000, CacheWrite: 7800}
	b.Observe(ModelDelta{Model: "claude-fable-5", CostUSD: 0.4321, Basis: BasisManaged, Tokens: tok})
	got, basis, ok := b.Estimate("claude-fable-5", tok)
	if !ok || !approx(got, 0.4321) || basis != BasisManaged {
		t.Fatalf("Estimate = %v %q %v, want 0.4321 managed true", got, basis, ok)
	}
	double := Tokens{Input: 24, Output: 680, CacheRead: 112_000, CacheWrite: 15600}
	if got, _, _ := b.Estimate("claude-fable-5", double); !approx(got, 0.8642) {
		t.Fatalf("Estimate(2x) = %v, want 0.8642", got)
	}
}

// A model whose rows leave a token column empty cannot be fitted; it falls
// back to the weighted ratio rather than to a singular solve.
func TestRateBook_UnfittableFallsBackToWeightedRatio(t *testing.T) {
	b := NewRateBook()
	total, weighted := 0.0, 0.0
	for i := range minFitObservations + 2 {
		tok := Tokens{Input: int64(100 * (i + 1)), Output: int64(10 * (i + 1))}
		c := 0.001 * float64(i+1)
		b.Observe(ModelDelta{Model: "m", CostUSD: c, Tokens: tok})
		total += c
		weighted += float64(tok.Input) + 5*float64(tok.Output)
	}
	probe := Tokens{Input: 1000, CacheRead: 10_000}
	want := total / weighted * (1000 + 0.1*10_000)
	if got, _, ok := b.Estimate("m", probe); !ok || !approx(got, want) {
		t.Fatalf("Estimate = %v ok=%v, want %v", got, ok, want)
	}
}

// A fit that needs a negative price is not a price list; the book falls
// back to the weighted ratio instead of pricing some mixes below zero.
func TestRateBook_NegativeFitFallsBackToWeightedRatio(t *testing.T) {
	b := NewRateBook()
	total, weighted := 0.0, 0.0
	for i := range minFitObservations {
		tok := varied(i)
		c := 10e-6*float64(tok.Output) - 1e-6*float64(tok.Input) + 0.1e-6*float64(tok.CacheRead) + 1e-6*float64(tok.CacheWrite)
		b.Observe(ModelDelta{Model: "m", CostUSD: c, Tokens: tok})
		total += c
		weighted += float64(tok.Input) + 5*float64(tok.Output) + 0.1*float64(tok.CacheRead) + 1.25*float64(tok.CacheWrite)
	}
	probe := Tokens{Input: 1_000_000}
	if got, _, ok := b.Estimate("m", probe); !ok || !approx(got, total/weighted*1_000_000) {
		t.Fatalf("Estimate = %v ok=%v, want the weighted-ratio price %v", got, ok, total/weighted*1_000_000)
	}
}

// Rows no price list explains (here, costs unrelated to their tokens) can
// still fit non-negative prices that miss the observed total by ~95%; the
// book distrusts such a fit and falls back to the weighted ratio.
func TestRateBook_FitMissingTheTotalFallsBack(t *testing.T) {
	rows := [][5]float64{
		{1, 1, 10, 1, 0.1}, {1000, 10, 100, 100, 0.01}, {100, 1, 1, 1, 0.001}, {10, 1, 10, 1000, 0.001},
		{1, 1000, 1, 10, 0.001}, {10, 1000, 1000, 100, 0.01}, {1, 10, 1, 10, 1}, {1, 10, 1000, 1000, 0.01},
	}
	b := NewRateBook()
	total, weighted := 0.0, 0.0
	for _, r := range rows {
		tok := Tokens{Input: int64(r[0]), Output: int64(r[1]), CacheRead: int64(r[2]), CacheWrite: int64(r[3])}
		b.Observe(ModelDelta{Model: "m", CostUSD: r[4], Tokens: tok})
		total += r[4]
		weighted += r[0] + 5*r[1] + 0.1*r[2] + 1.25*r[3]
	}
	if got, _, ok := b.Estimate("m", Tokens{Output: 100}); !ok || !approx(got, total/weighted*500) {
		t.Fatalf("Estimate = %v ok=%v, want the weighted-ratio price %v", got, ok, total/weighted*500)
	}
}

func TestRateBook_KeysProviderRevisionAndContextVariantsTogether(t *testing.T) {
	for raw, want := range map[string]string{
		"claude-opus-5":                                "claude-opus-5",
		"global.anthropic.claude-opus-5[1m]":           "claude-opus-5",
		"us.anthropic.claude-opus-5-v1:0":              "claude-opus-5",
		"anthropic.claude-3-5-sonnet-20241022-v2:0":    "claude-3-5-sonnet",
		"claude-opus-4-1@20250805":                     "claude-opus-4-1",
		"eu.anthropic.claude-sonnet-4-5-20250929-v1:0": "claude-sonnet-4-5",
		" Claude-Haiku-4-5 ":                           "claude-haiku-4-5",
		"claude-opus-4-6-vision":                       "claude-opus-4-6-vision",
		"claude-opus-4-6-1234567":                      "claude-opus-4-6-1234567",
	} {
		if got := RateKey(raw); got != want {
			t.Errorf("RateKey(%q) = %q, want %q", raw, got, want)
		}
	}

	b := NewRateBook()
	tok := Tokens{Input: 10, Output: 10}
	b.Observe(ModelDelta{RawModel: "global.anthropic.claude-opus-5[1m]", CostUSD: 0.01, Tokens: tok})
	for _, m := range []string{"claude-opus-5", "us.anthropic.claude-opus-5-v1:0", "claude-opus-5[1m]"} {
		if got, _, ok := b.Estimate(m, tok); !ok || !approx(got, 0.01) {
			t.Errorf("Estimate(%q) = %v ok=%v, want the rate learned under the raw key", m, got, ok)
		}
	}
}

func TestRateBook_UnseenNilAndUnpricedRows(t *testing.T) {
	var nilBook *RateBook
	nilBook.Observe(ModelDelta{Model: "m", CostUSD: 1, Tokens: Tokens{Input: 1}})
	if _, _, ok := nilBook.Estimate("m", Tokens{Input: 1}); ok {
		t.Fatal("nil book must report no rate")
	}
	b := NewRateBook()
	b.Observe(ModelDelta{Model: "free", Tokens: Tokens{Input: 100}})
	b.Observe(ModelDelta{Model: "search-only", CostUSD: 0.01, Tokens: Tokens{WebSearch: 1}})
	b.Observe(ModelDelta{Model: invalidIdent, CostUSD: 0.01, Tokens: Tokens{Input: 1}})
	for _, m := range []string{"free", "search-only", invalidIdent, "never-seen"} {
		if got, _, ok := b.Estimate(m, Tokens{Input: 100}); ok || got != 0 {
			t.Errorf("Estimate(%q) = %v ok=%v, want no rate", m, got, ok)
		}
	}
}

func TestRateBook_BasisIsWorstObserved(t *testing.T) {
	b := NewRateBook()
	b.Observe(ModelDelta{Model: "m", CostUSD: 0.1, Basis: BasisList, Tokens: Tokens{Input: 10}})
	b.Observe(ModelDelta{Model: "m", CostUSD: 0.1, Basis: BasisUnknown, Tokens: Tokens{Input: 10}})
	b.Observe(ModelDelta{Model: "m", CostUSD: 0.1, Basis: BasisManaged, Tokens: Tokens{Input: 10}})
	if _, basis, _ := b.Estimate("m", Tokens{Input: 1}); basis != BasisUnknown {
		t.Fatalf("basis = %q, want unknown", basis)
	}
}

// The book is seeded at open from recent CLI-priced turn rows only: a partial
// entry's estimate must not teach the book its own guess, and rows older than
// rateSeedDays are not learned from.
func TestStore_SeedsRatesFromRecentTurnRows(t *testing.T) {
	s, dir := newTestStore(t, t0)
	if s.Rates() == nil {
		t.Fatal("enabled store has no rate book")
	}
	tok := Tokens{Input: 10, Output: 10}
	s.Append(mk(t0, SourceSession, UnitUSD, 0.2, ModelDelta{Model: "turn-model", CostUSD: 0.2, Tokens: tok}))
	s.Append(mk(t0, SourceCronLocal, UnitUSD, 0.3, ModelDelta{Model: "cron-model", CostUSD: 0.3, Tokens: tok}))
	partial := mk(t0, SourceSession, UnitUSD, 0.4, ModelDelta{Model: "partial-model", CostUSD: 0.4, Tokens: tok})
	partial.Kind = KindPartial
	s.Append(partial)
	old := t0.Add(-(rateSeedDays + 1) * 24 * time.Hour)
	s.Append(mk(old, SourceSession, UnitUSD, 0.5, ModelDelta{Model: "old-model", CostUSD: 0.5, Tokens: tok}))
	s.Close()

	re := NewStore(dir, Options{Now: func() time.Time { return t0 }})
	t.Cleanup(re.Close)
	for m, want := range map[string]float64{"turn-model": 0.2, "cron-model": 0.3} {
		if got, _, ok := re.Rates().Estimate(m, tok); !ok || !approx(got, want) {
			t.Errorf("seeded %s = %v ok=%v, want %v", m, got, ok, want)
		}
	}
	for _, m := range []string{"partial-model", "old-model"} {
		if _, _, ok := re.Rates().Estimate(m, tok); ok {
			t.Errorf("%s must not seed the rate book", m)
		}
	}
	if (&Store{disabled: true}).Rates() != nil {
		t.Error("disabled store must have no rate book")
	}
}
