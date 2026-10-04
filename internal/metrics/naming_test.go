package metrics

import (
	"sort"
	"strings"
	"testing"
)

func TestName_BuildsConventionCompliant(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sub  Subsystem
		name string
		kind Kind
		want string
	}{
		{SubsystemSession, "create", KindCounter, "naozhi_session_create_total"},
		{SubsystemCron, "run_failed", KindCounter, "naozhi_cron_run_failed_total"},
		{SubsystemCron, "run", KindGaugeInflight, "naozhi_cron_run_inflight"},
		{SubsystemStartup, "phase_config", KindGaugeMillis, "naozhi_startup_phase_config_ms"},
		{SubsystemAutoChain, "spawn_attach", KindCounter, "naozhi_auto_chain_spawn_attach_total"},
		{SubsystemUpstream, "reqsem", KindGaugeInflight, "naozhi_upstream_reqsem_inflight"},
	}
	for _, c := range cases {
		got, err := Name(c.sub, c.name, c.kind)
		if err != nil {
			t.Errorf("Name(%q,%q,%d) error: %v", c.sub, c.name, c.kind, err)
			continue
		}
		if got != c.want {
			t.Errorf("Name(%q,%q,%d) = %q, want %q", c.sub, c.name, c.kind, got, c.want)
		}
		if !ValidName(got) {
			t.Errorf("ValidName(%q) = false, want true", got)
		}
	}
}

func TestName_RejectsBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sub  Subsystem
		name string
		kind Kind
	}{
		{Subsystem("bogus"), "create", KindCounter},     // unknown subsystem
		{SubsystemSession, "Create", KindCounter},       // uppercase
		{SubsystemSession, "_create", KindCounter},      // leading underscore
		{SubsystemSession, "create_", KindCounter},      // trailing underscore
		{SubsystemSession, "create_total", KindCounter}, // already has suffix
		{SubsystemSession, "create", Kind(99)},          // unknown kind
	}
	for _, c := range cases {
		if got, err := Name(c.sub, c.name, c.kind); err == nil {
			t.Errorf("Name(%q,%q,%d) = %q, want error", c.sub, c.name, c.kind, got)
		}
	}
}

func TestValidName_RejectsNonConforming(t *testing.T) {
	t.Parallel()
	bad := []string{
		"foo_bar_total",                 // wrong prefix
		"naozhi_unknownsub_x_total",     // unknown subsystem
		"naozhi_session",                // no name/suffix
		"naozhi_session_create_widgets", // unknown suffix
	}
	for _, b := range bad {
		if ValidName(b) {
			t.Errorf("ValidName(%q) = true, want false", b)
		}
	}
}

// legacyNonConforming lists registered names that predate the convention and
// stay as they are: /debug/vars scrapes and docs pin them. Each entry must
// still be registered and still fail ValidName, so the list cannot go stale.
var legacyNonConforming = map[string]string{
	"naozhi_upstream_connector_backoff_millis":         "_millis suffix; named in docs/rfc/node-pairing.md",
	"naozhi_cron_watchdog_parked_interrupt_goroutines": "_goroutines suffix; pairs with naozhi_cron_watchdog_interrupt_timeout_total",
}

// TestRegisteredMetricsConformToConvention asserts every naozhi_* metric
// registered anywhere in the repo passes ValidName (#622), so a new metric
// with an unknown subsystem or a stray suffix fails the build.
func TestRegisteredMetricsConformToConvention(t *testing.T) {
	t.Parallel()

	decls := declaredMetricNames(t)
	var nonConforming []string
	for name, file := range decls {
		if _, legacy := legacyNonConforming[name]; !legacy && !ValidName(name) {
			nonConforming = append(nonConforming, name+" ("+file+")")
		}
	}
	sort.Strings(nonConforming)
	if len(nonConforming) > 0 {
		t.Errorf("metric names violating the naozhi_<subsystem>_<name>_<suffix> convention:\n  %s\nadd the subsystem to KnownSubsystems or fix the suffix.", strings.Join(nonConforming, "\n  "))
	}

	for name := range legacyNonConforming {
		if _, ok := decls[name]; !ok {
			t.Errorf("legacyNonConforming lists %s, which is registered nowhere; delete the entry", name)
		} else if ValidName(name) {
			t.Errorf("legacyNonConforming lists %s, which already passes ValidName; delete the entry", name)
		}
	}
}
