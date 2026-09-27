package procmeter

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestMeter_MeteringFastPath: with no metering rows, Metering answers from the
// length probe alone; after a merge the probe tracks the merged row count,
// not the incoming batch size.
func TestMeter_MeteringFastPath(t *testing.T) {
	t.Parallel()
	var m Meter
	if got := m.meteringLen.Load(); got != 0 {
		t.Errorf("zero-value length = %d, want 0", got)
	}
	if got := m.Metering(); got != nil {
		t.Errorf("zero-value Metering = %v, want nil", got)
	}

	m.ApplyMetadata(&clievent.EventMetadata{MeteringUsage: []clievent.MeteringEntry{{Value: 0.01, Unit: "credit", UnitPlural: "credits"}}})
	if got := m.meteringLen.Load(); got != 1 {
		t.Errorf("after the first merge length = %d, want 1", got)
	}
	m.ApplyMetadata(&clievent.EventMetadata{MeteringUsage: []clievent.MeteringEntry{{Value: 100, Unit: "token", UnitPlural: "tokens"}}})
	if got := m.meteringLen.Load(); got != 2 {
		t.Errorf("after a second unit length = %d, want 2", got)
	}
	m.ApplyMetadata(&clievent.EventMetadata{MeteringUsage: []clievent.MeteringEntry{{Value: 0.02, Unit: "credit", UnitPlural: "credits"}}})
	if got := m.meteringLen.Load(); got != 2 {
		t.Errorf("after a same-unit merge length = %d, want 2 (no growth)", got)
	}
	rows := m.Metering()
	if len(rows) != 2 || rows[0].Unit != "credit" || rows[0].Value != 0.03 {
		t.Errorf("rows = %+v, want credit summed to 0.03 and token", rows)
	}
	if got := m.MeteringGen(); got != 3 {
		t.Errorf("MeteringGen = %d after three metering writes, want 3", got)
	}
}

// TestMeter_MeteringRowsAreBounded: a upstream inventing a unit per frame
// stops adding rows at the bound; known units keep accumulating.
func TestMeter_MeteringRowsAreBounded(t *testing.T) {
	t.Parallel()
	var m Meter
	for i := 0; i < maxMeteringUnits+5; i++ {
		m.ApplyMetadata(&clievent.EventMetadata{MeteringUsage: []clievent.MeteringEntry{{Value: 1, Unit: string(rune('a' + i))}}})
	}
	m.ApplyMetadata(&clievent.EventMetadata{MeteringUsage: []clievent.MeteringEntry{{Value: 1, Unit: "a"}}})
	rows := m.Metering()
	if len(rows) != maxMeteringUnits {
		t.Fatalf("rows = %d, want the bound %d", len(rows), maxMeteringUnits)
	}
	if rows[0].Value != 2 {
		t.Errorf("a known unit past the bound = %v, want 2", rows[0].Value)
	}
}

// TestMeter_ShadowAccumulatesUntilResult: assistant frames' usage sums per
// turn, the result frame clears it, TakeShadow resets it.
func TestMeter_ShadowAccumulatesUntilResult(t *testing.T) {
	var m Meter
	asst := func(in, out int64, model string) clievent.Event {
		return clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{Model: model,
			Usage: &clievent.MessageUsage{InputTokens: in, OutputTokens: out, CacheCreationInputTokens: 10}}}
	}
	m.TrackShadow(asst(5, 7, "us.anthropic.claude-fable-5-1[1m]"))
	m.TrackShadow(clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{}}) // no usage: ignored
	m.TrackShadow(asst(1, 2, ""))
	u := m.TakeShadow()
	if u.Input != 6 || u.Output != 9 || u.CacheWrite != 20 || u.Model != "us.anthropic.claude-fable-5-1[1m]" || u.IsZero() {
		t.Fatalf("shadow = %+v", u)
	}
	if !m.TakeShadow().IsZero() {
		t.Fatal("TakeShadow must clear the account")
	}
	m.TrackShadow(asst(3, 3, "m"))
	m.TrackShadow(clievent.Event{Type: "result", CostUSD: 1})
	if !m.TakeShadow().IsZero() {
		t.Fatal("a result frame must clear the account (its modelUsage supersedes it)")
	}
}

// TestMeter_GuardsAndGates: a metadata frame that omits a field keeps the
// earlier value; the effort seed never overrides a reported tier; the live
// version reports a change once.
func TestMeter_GuardsAndGates(t *testing.T) {
	t.Parallel()
	var m Meter
	m.ApplyMetadata(&clievent.EventMetadata{ContextUsagePercent: 40, TurnDurationMs: 900, Effort: "high"})
	m.ApplyMetadata(&clievent.EventMetadata{})
	if m.ContextUsagePercent() != 40 || m.TurnDurationMs() != 900 || m.Effort() != "high" {
		t.Errorf("an empty frame regressed a value: ctx=%v dur=%v effort=%q", m.ContextUsagePercent(), m.TurnDurationMs(), m.Effort())
	}
	m.SeedEffort("low")
	if m.Effort() != "high" {
		t.Errorf("the spawn pin overrode the reported tier: %q", m.Effort())
	}
	var fresh Meter
	fresh.SeedEffort("max")
	if fresh.Effort() != "max" {
		t.Errorf("the spawn pin did not seed an unset tier: %q", fresh.Effort())
	}
	if !m.SetLiveVersion("2.1.0") || m.SetLiveVersion("2.1.0") || !m.SetLiveVersion("2.2.0") {
		t.Error("SetLiveVersion must report a change exactly when the version changes")
	}
	if m.SetLiveVersion("") || m.LiveVersion() != "2.2.0" {
		t.Error("an empty version must be ignored")
	}
	m.SetModel("m1")
	m.SetModel("")
	if m.Model() != "" {
		t.Errorf("SetModel(\"\") must clear the model, got %q", m.Model())
	}
	m.RecordResultCost(1.25)
	if m.TotalCost() != 1.25 {
		t.Errorf("TotalCost = %v, want 1.25", m.TotalCost())
	}
}

// TestMeter_MeteringLearnsAUnitsPlural: a unit first reported without its
// plural label takes the label a later frame carries.
func TestMeter_MeteringLearnsAUnitsPlural(t *testing.T) {
	t.Parallel()
	var m Meter
	m.ApplyMetadata(&clievent.EventMetadata{MeteringUsage: []clievent.MeteringEntry{{Value: 1, Unit: "credit"}}})
	m.ApplyMetadata(&clievent.EventMetadata{MeteringUsage: []clievent.MeteringEntry{{Value: 1, Unit: "credit", UnitPlural: "credits"}}})
	m.ApplyMetadata(&clievent.EventMetadata{MeteringUsage: []clievent.MeteringEntry{{Value: 1, Unit: "credit"}}})
	if rows := m.Metering(); len(rows) != 1 || rows[0].UnitPlural != "credits" || rows[0].Value != 3 {
		t.Errorf("rows = %+v, want one credit row of 3 labelled credits", rows)
	}
}
