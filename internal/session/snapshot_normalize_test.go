package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestCostUnitForBackend pins the dashboard-facing unit selection for each
// known backend. Adding a new backend requires extending this table at the
// same time as profile.RegisterDefaults so the dashboard UI gets a stable
// label out of the box.
func TestCostUnitForBackend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		backend string
		want    string
	}{
		{"", "USD"}, // legacy stores predating Backend field — claude-only
		{"claude", "USD"},
		{"kiro", "credits"},
		{"unknown-backend", ""}, // explicit empty so dashboard hides the cell
	}
	for _, c := range cases {
		got := costUnitForBackend(c.backend)
		if got != c.want {
			t.Errorf("costUnitForBackend(%q) = %q, want %q", c.backend, got, c.want)
		}
	}
}

// TestSnapshot_NormalizeFields_LiveProcess exercises the proc-attached path
// of Snapshot — the three normalize fields are sourced from Process accessors
// so a kiro session with metadata flowing in surfaces correct values without
// touching the cron / IM stubs.
func TestSnapshot_NormalizeFields_LiveProcess(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:direct:alice:general"}
	s.SetBackend("kiro")
	proc := newMetadataTestProcess(42.5, 1234, []cli.MeteringEntry{
		{Value: 0.05, Unit: "credit", UnitPlural: "credits"},
	})
	proc.EffortVal = "xhigh"
	s.storeProcess(proc)

	snap := s.Snapshot()

	if snap.CostUnit != "credits" {
		t.Errorf("CostUnit = %q, want credits (kiro backend)", snap.CostUnit)
	}
	if snap.ContextUsagePercent != 42.5 {
		t.Errorf("ContextUsagePercent = %v, want 42.5", snap.ContextUsagePercent)
	}
	if snap.TurnDurationMs != 1234 {
		t.Errorf("TurnDurationMs = %v, want 1234", snap.TurnDurationMs)
	}
	if len(snap.MeteringUsage) != 1 || snap.MeteringUsage[0].Value != 0.05 {
		t.Errorf("MeteringUsage = %+v, want 1 entry", snap.MeteringUsage)
	}
	if snap.Effort != "xhigh" {
		t.Errorf("Effort = %q, want xhigh", snap.Effort)
	}
}

// TestSnapshot_EffortWireTag pins the JSON field name the dashboard reads.
// Without this, renaming the tag (or dropping it) compiles, passes every other
// Go test, and only shows up as a header tag that silently never appears —
// dashboard.js reads `effort` off the /api/sessions row, and
// sessions_shape_test.go only enforces the required key set, not this field.
func TestSnapshot_EffortWireTag(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(SessionSnapshot{Effort: "max"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"effort":"max"`) {
		t.Errorf(`marshalled snapshot missing "effort":"max"; got %s`, b)
	}
	// omitempty: a backend that reports no tier must not add the key at all,
	// so the frontend's truthiness gate hides the tag instead of rendering "".
	b2, err := json.Marshal(SessionSnapshot{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if strings.Contains(string(b2), "effort") {
		t.Errorf("empty Effort must be omitted (omitempty); got %s", b2)
	}
}

// TestSnapshot_NormalizeFields_NoProcess covers the dead/unsuspended branch:
// a session that has no live Process still reports CostUnit (so the dashboard
// can still render the right unit label), while the three live fields stay
// at zero.
func TestSnapshot_NormalizeFields_NoProcess(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:direct:alice:general"}
	s.SetBackend("kiro")

	snap := s.Snapshot()

	if snap.CostUnit != "credits" {
		t.Errorf("CostUnit must still resolve from Backend even without proc; got %q", snap.CostUnit)
	}
	if snap.ContextUsagePercent != 0 {
		t.Errorf("ContextUsagePercent should be 0 without proc; got %v", snap.ContextUsagePercent)
	}
	if snap.TurnDurationMs != 0 {
		t.Errorf("TurnDurationMs should be 0 without proc; got %v", snap.TurnDurationMs)
	}
	if snap.MeteringUsage != nil {
		t.Errorf("MeteringUsage should be nil without proc; got %v", snap.MeteringUsage)
	}
	// Effort is a runtime observation, so unlike CostUnit it does NOT survive
	// eviction — there is no persisted tier to fall back on (effort is never
	// written to sessions.json). The dashboard tag hides in this state.
	if snap.Effort != "" {
		t.Errorf("Effort should be empty without proc; got %q", snap.Effort)
	}
}

// TestSnapshot_LegacyEmptyBackend_DefaultsToUSD locks the back-compat
// contract: stores predating the Backend field MUST default to claude/USD.
func TestSnapshot_LegacyEmptyBackend_DefaultsToUSD(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:direct:alice:general"}
	// no SetBackend call — simulates a legacy session restored from
	// pre-multi-backend sessions.json
	snap := s.Snapshot()
	if snap.CostUnit != "USD" {
		t.Errorf("legacy empty backend should default to USD, got %q", snap.CostUnit)
	}
}

// metadataTestProcess is a TestProcess wrapper that returns custom values
// from the normalize-layer accessors. Used only by snapshot_normalize_test.go.
type metadataTestProcess struct {
	*TestProcess
	contextPct float64
	turnMs     int64
	metering   []cli.MeteringEntry
}

func newMetadataTestProcess(pct float64, ms int64, metering []cli.MeteringEntry) *metadataTestProcess {
	return &metadataTestProcess{
		TestProcess: NewTestProcess(),
		contextPct:  pct,
		turnMs:      ms,
		metering:    metering,
	}
}

func (m *metadataTestProcess) ContextUsagePercent() float64 { return m.contextPct }
func (m *metadataTestProcess) TurnDurationMs() int64        { return m.turnMs }
func (m *metadataTestProcess) MeteringUsage() []cli.MeteringEntry {
	if m.metering == nil {
		return nil
	}
	out := make([]cli.MeteringEntry, len(m.metering))
	copy(out, m.metering)
	return out
}
