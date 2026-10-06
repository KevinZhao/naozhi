package costledger

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func validEntry() Entry {
	return Entry{Source: SourceSession, Kind: KindTurn, Backend: "claude", Unit: UnitUSD, Amount: 0.5, Basis: BasisList}
}

func TestNormalize_RejectsInvalidEnumsAndEmpty(t *testing.T) {
	bad := []Entry{
		func() Entry { e := validEntry(); e.Source = "elsewhere"; return e }(),
		func() Entry { e := validEntry(); e.Unit = "EUR"; return e }(),
		func() Entry { e := validEntry(); e.Kind = "guess"; return e }(),
		func() Entry { e := validEntry(); e.Amount = 0; return e }(),
		func() Entry { e := validEntry(); e.Amount = -1; return e }(),
		func() Entry { e := validEntry(); e.Backend = ""; return e }(),
	}
	for i, e := range bad {
		if e.normalize() {
			t.Errorf("case %d: expected rejection of %+v", i, e)
		}
	}
	zeroWithModels := validEntry()
	zeroWithModels.Amount = 0
	zeroWithModels.Models = []ModelDelta{{Model: "m", Tokens: Tokens{Input: 1}}}
	if !zeroWithModels.normalize() {
		t.Error("zero amount with model rows must be accepted (partial/token-only)")
	}
}

// An adjust entry is the one kind allowed to take money back: its negative
// Amount survives, while any other kind's is clamped or rejected.
func TestNormalize_OnlyAdjustKeepsANegativeAmount(t *testing.T) {
	adj := validEntry()
	adj.Kind, adj.Amount = KindAdjust, -929.98
	if ok := adj.normalize(); !ok || adj.Amount != -929.98 {
		t.Fatalf("adjust: accepted=%v amount=%v, want -929.98 kept", ok, adj.Amount)
	}
	zero := validEntry()
	zero.Kind, zero.Amount = KindAdjust, 0
	if zero.normalize() {
		t.Error("an adjust of nothing must be rejected")
	}
	turn := validEntry()
	turn.Amount = -5
	turn.Models = []ModelDelta{{Model: "m", Tokens: Tokens{Input: 1}}}
	if !turn.normalize() || turn.Amount != 0 {
		t.Errorf("turn with rows: amount = %v, want clamped to 0", turn.Amount)
	}
}

func TestNormalize_BasisAndIdentSanitized(t *testing.T) {
	e := validEntry()
	e.Basis = "contract"
	e.Models = []ModelDelta{
		{Model: strings.Repeat("x", maxIdentLen+1), RawModel: "ok\x01bad", Provider: string([]byte{0xff, 0xfe}), Basis: "weird"},
	}
	if !e.normalize() {
		t.Fatal("expected acceptance")
	}
	if e.Basis != BasisUnknown || e.Models[0].Basis != BasisUnknown {
		t.Errorf("basis not normalised: %q / %q", e.Basis, e.Models[0].Basis)
	}
	m := e.Models[0]
	if m.Model != invalidIdent || m.RawModel != invalidIdent || m.Provider != invalidIdent {
		t.Errorf("idents not sanitised: %+v", m)
	}
	if e.TS.IsZero() || e.TS.Location().String() != "UTC" {
		t.Errorf("TS not stamped in UTC: %v", e.TS)
	}
}

func TestNormalize_CapsModels(t *testing.T) {
	e := validEntry()
	for i := 0; i < MaxModels+5; i++ {
		e.Models = append(e.Models, ModelDelta{Model: "m"})
	}
	e.normalize()
	if len(e.Models) != MaxModels {
		t.Fatalf("models = %d, want %d", len(e.Models), MaxModels)
	}
}

// Rows past the cap fold into one "other" row, so the rows still sum to the
// entry's Amount; Amount itself is never trimmed. A folded row's invalid basis
// still surfaces as unknown, as it would on a kept row.
func TestNormalize_FoldsOverflowModelsIntoOther(t *testing.T) {
	e := validEntry()
	var cost float64
	var tok Tokens
	for i := 0; i < MaxModels+4; i++ {
		m := ModelDelta{Model: fmt.Sprintf("m%d", i), CostUSD: 0.01 * float64(i+1), Basis: BasisList,
			Tokens: Tokens{Input: int64(i), Output: 10, CacheRead: 2, CacheWrite: 1, Thinking: 3, WebSearch: 1}}
		if i == MaxModels+2 {
			m.Basis = BasisManaged
		}
		if i == MaxModels {
			m.Basis = "weird"
		}
		cost += m.CostUSD
		tok = tok.add(m.Tokens)
		e.Models = append(e.Models, m)
	}
	e.Amount = cost
	if !e.normalize() || len(e.Models) != MaxModels || e.Amount != cost {
		t.Fatalf("normalize: %d models, amount %v; want %d and %v", len(e.Models), e.Amount, MaxModels, cost)
	}
	var gotCost float64
	var gotTok Tokens
	for _, m := range e.Models {
		gotCost += m.CostUSD
		gotTok = gotTok.add(m.Tokens)
	}
	last := e.Models[MaxModels-1]
	if last.Model != OtherModel || last.Basis != BasisUnknown || e.Models[MaxModels-2].Model != fmt.Sprintf("m%d", MaxModels-2) {
		t.Fatalf("last rows = %+v, %+v: want the kept rows in order, then other at the worst basis (an invalid one counts as unknown)", e.Models[MaxModels-2], last)
	}
	if math.Abs(gotCost-cost) > 1e-12 || gotTok != tok {
		t.Fatalf("rows sum to %v / %+v, want %v / %+v", gotCost, gotTok, cost, tok)
	}
}

func TestSanitizeIdent_KeepsRealModelIDs(t *testing.T) {
	for _, s := range []string{"us.anthropic.claude-fable-5-1[1m]", "claude-opus-5", "bedrock", "模型"} {
		if got := sanitizeIdent(s); got != s {
			t.Errorf("sanitizeIdent(%q) = %q", s, got)
		}
	}
}

// A row's mark survives the store's write and read paths; a row without one
// writes no "mark" key, and a line from before marks decodes with none.
func TestEntry_MarkRoundTripsAndStaysOptional(t *testing.T) {
	s, _ := newTestStore(t, t0)
	marked := mk(t0, SourceSession, UnitUSD, 0.25)
	marked.Mark = &SessionMark{Spent: 3.5, Cum: 1.25, Born: 1700000000123456789}
	if !s.Append(marked) || !s.Append(mk(t0, SourceSession, UnitUSD, 0.5)) {
		t.Fatal("append rejected")
	}
	s.Close()
	got, err := s.Entries(Query{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("entries = %+v, %v", got, err)
	}
	for _, e := range got {
		switch {
		case e.Amount == 0.5 && e.Mark != nil:
			t.Fatalf("unmarked row decoded with mark %+v", *e.Mark)
		case e.Amount == 0.25 && (e.Mark == nil || *e.Mark != *marked.Mark):
			t.Fatalf("marked row decoded with mark %+v, want %+v", e.Mark, *marked.Mark)
		}
	}

	line, err := json.Marshal(validEntry())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), `"mark"`) {
		t.Fatalf("unmarked row encodes a mark: %s", line)
	}
	var legacy Entry
	if err := json.Unmarshal([]byte(`{"ts":"2026-09-05T00:00:00Z","source":"session","kind":"turn","backend":"claude","unit":"USD","amount":1}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Mark != nil || legacy.Amount != 1 {
		t.Fatalf("legacy line = %+v, want amount 1 and no mark", legacy)
	}
}
