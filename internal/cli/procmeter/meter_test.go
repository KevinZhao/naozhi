package procmeter

import (
	"fmt"
	"reflect"
	"testing"
	"time"

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

func asstFrame(id, model string, in, out, cacheWrite int64) clievent.Event {
	return clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{ID: id, Model: model,
		Usage: &clievent.MessageUsage{InputTokens: in, OutputTokens: out, CacheCreationInputTokens: cacheWrite}}}
}

// TestMeter_ShadowAccumulatesUntilResult: distinct messages' usage sums per
// turn, a frame naming no model joins the last one named, the result frame
// clears the account, TakeShadow resets it.
func TestMeter_ShadowAccumulatesUntilResult(t *testing.T) {
	t.Parallel()
	var m Meter
	m.TrackShadow(asstFrame("msg_a", "us.anthropic.claude-fable-5-1[1m]", 5, 7, 10), 1)
	m.TrackShadow(clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{}}, 2) // no usage: ignored
	m.TrackShadow(asstFrame("msg_b", "", 1, 2, 10), 3)
	u := m.TakeShadow()
	want := []clievent.ShadowModel{{Model: "us.anthropic.claude-fable-5-1[1m]", Input: 6, Output: 9, CacheWrite: 20}}
	if !reflect.DeepEqual(u.Models, want) || u.IsZero() {
		t.Fatalf("shadow = %+v, want %+v", u.Models, want)
	}
	if !m.TakeShadow().IsZero() {
		t.Fatal("TakeShadow must clear the account")
	}
	m.TrackShadow(asstFrame("msg_c", "m", 3, 3, 0), 4)
	m.TrackShadow(clievent.Event{Type: "result", CostUSD: 1}, 5)
	if !m.TakeShadow().IsZero() {
		t.Fatal("a result frame must clear the account (its modelUsage supersedes it)")
	}
}

// TestMeter_ShadowCountsEachMessageOnce: the CLI writes one frame per content
// block, each repeating the message's id and input/cache usage, with output
// growing; the message counts once, at its final output.
func TestMeter_ShadowCountsEachMessageOnce(t *testing.T) {
	t.Parallel()
	var m Meter
	m.TrackShadow(asstFrame("msg_1", "opus", 3, 8, 21584), 1)
	m.TrackShadow(asstFrame("msg_1", "opus", 3, 224, 21584), 2)
	m.TrackShadow(asstFrame("msg_2", "opus", 1, 50, 0), 3)
	m.TrackShadow(asstFrame("msg_2", "opus", 1, 40, 0), 4) // a smaller late frame never lowers it
	want := []clievent.ShadowModel{{Model: "opus", Input: 4, Output: 274, CacheWrite: 21584}}
	if got := m.TakeShadow().Models; !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow = %+v, want %+v", got, want)
	}
}

// TestMeter_ShadowPerModel: each model gets its own row in first-seen order,
// and a message keeps the model its first frame named.
func TestMeter_ShadowPerModel(t *testing.T) {
	t.Parallel()
	var m Meter
	m.TrackShadow(asstFrame("msg_1", "opus", 10, 1, 0), 1)
	m.TrackShadow(asstFrame("msg_2", "haiku", 20, 2, 0), 2)
	m.TrackShadow(asstFrame("msg_1", "", 10, 5, 0), 3)
	m.TrackShadow(asstFrame("msg_3", "opus", 30, 3, 0), 4)
	want := []clievent.ShadowModel{{Model: "opus", Input: 40, Output: 8}, {Model: "haiku", Input: 20, Output: 2}}
	if got := m.TakeShadow().Models; !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow = %+v, want %+v", got, want)
	}
}

// TestMeter_ShadowBounds: models past maxShadowModels share one unnamed row,
// and message ids past maxShadowMessages restart the memory without losing
// usage.
func TestMeter_ShadowBounds(t *testing.T) {
	t.Parallel()
	var m Meter
	for i := range maxShadowModels + 3 {
		m.TrackShadow(asstFrame(fmt.Sprint("msg_", i), fmt.Sprint("model-", i), 1, 0, 0), 1)
	}
	rows := m.TakeShadow().Models
	if len(rows) != maxShadowModels+1 || rows[maxShadowModels] != (clievent.ShadowModel{Input: 3}) {
		t.Fatalf("rows = %d, overflow row = %+v", len(rows), rows[len(rows)-1])
	}
	for i := range maxShadowMessages + 5 {
		m.TrackShadow(asstFrame(fmt.Sprint("msg_", i), "m", 1, 0, 0), 1)
	}
	if n := len(m.shadowMsgs); n != 5 {
		t.Fatalf("remembered ids = %d, want 5 after the restart", n)
	}
	if got := m.TakeShadow().Models; len(got) != 1 || got[0].Input != maxShadowMessages+5 {
		t.Fatalf("shadow = %+v", got)
	}
}

// TestMeter_LastResultAt: zero before a result frame, then the receive time
// of the latest one; assistant frames never move it.
func TestMeter_LastResultAt(t *testing.T) {
	t.Parallel()
	var m Meter
	if !m.LastResultAt().IsZero() {
		t.Fatal("LastResultAt before any result must be zero")
	}
	m.TrackShadow(clievent.Event{Type: "result"}, 1_700_000_000_123)
	m.TrackShadow(asstFrame("msg_1", "m", 1, 1, 0), 1_700_000_000_999)
	if got := m.LastResultAt(); !got.Equal(time.UnixMilli(1_700_000_000_123)) {
		t.Fatalf("LastResultAt = %v", got)
	}
	m.TrackShadow(clievent.Event{Type: "result"}, 1_700_000_001_000)
	if got := m.LastResultAt(); !got.Equal(time.UnixMilli(1_700_000_001_000)) {
		t.Fatalf("LastResultAt after a second result = %v", got)
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
