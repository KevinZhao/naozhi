package metrics

import (
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
