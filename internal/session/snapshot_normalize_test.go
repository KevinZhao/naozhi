package session

import (
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
