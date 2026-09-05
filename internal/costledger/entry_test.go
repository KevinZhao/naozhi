package costledger

import (
	"strings"
	"testing"
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

func TestSanitizeIdent_KeepsRealModelIDs(t *testing.T) {
	for _, s := range []string{"us.anthropic.claude-fable-5-1[1m]", "claude-opus-5", "bedrock", "模型"} {
		if got := sanitizeIdent(s); got != s {
			t.Errorf("sanitizeIdent(%q) = %q", s, got)
		}
	}
}
