package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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

// TestRegisteredMetricsConformToConvention scans every metric name declared
// in the package source and asserts it passes ValidName. This is the
// regression guard for R247-ARCH-6 / #622: it pins the current name set to
// the codified convention so a new metric with a stray prefix or missing
// suffix fails the build instead of quietly adding a ninth ad-hoc shape.
func TestRegisteredMetricsConformToConvention(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	metricsDir := filepath.Dir(self)

	declRE := regexp.MustCompile(`(?:expvar\.NewInt|expvar\.NewMap|NewLabeledCounter|NewLabeledGauge)\("(naozhi_[a-z0-9_]+)"\)`)

	entries, err := os.ReadDir(metricsDir)
	if err != nil {
		t.Fatalf("read dir %s: %v", metricsDir, err)
	}
	names := map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(metricsDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range declRE.FindAllSubmatch(src, -1) {
			names[string(m[1])] = struct{}{}
		}
	}
	if len(names) == 0 {
		t.Fatal("no metric declarations matched — regex out of sync with source?")
	}

	var nonConforming []string
	for n := range names {
		if !ValidName(n) {
			nonConforming = append(nonConforming, n)
		}
	}
	sort.Strings(nonConforming)
	if len(nonConforming) > 0 {
		t.Errorf("metric names violating the naozhi_<subsystem>_<name>_<suffix> convention:\n  %s\nadd the subsystem to KnownSubsystems or fix the suffix.", strings.Join(nonConforming, "\n  "))
	}
}
