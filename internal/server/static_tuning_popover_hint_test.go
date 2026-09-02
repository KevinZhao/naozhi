// static_tuning_popover_hint_test.go — wiring pins for the model popover's
// empty-manifest hint.
//
// The hint must tell the operator something true per backend: ACP backends
// (kiro) report their manifest on the first session, so "wait for a session"
// is right; stream-json backends (claude) never report one, so the only ways
// to get a list are cli.backends[].models or the observed-models tier — the
// old single sentence told claude operators to wait for something that would
// never happen. Source-grep pins per the #388 guidance in
// static_ux_contract_test.go. docs/rfc/dashboard-model-effort-control.md §4.2
package server

import (
	"strings"
	"testing"
)

func TestDashboardJS_TuningPopoverEmptyManifestHint(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	for _, want := range []string{
		// tuningModelsForSession must surface the backend's wire protocol so
		// the popover can branch on it (BackendInfo.protocol: "acp" |
		// "stream-json").
		`protocol: entry`,
		// ACP branch keeps the first-session wording.
		`清单在该 backend 首次会话后可用`,
		// Non-ACP branch points at the config knob instead.
		`cli.backends[].models`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing popover hint wiring: %q", want)
		}
	}
	// The branch must key off the protocol value, not a hardcoded backend id.
	if !strings.Contains(js, `protocol === 'acp'`) {
		t.Error("popover hint must branch on protocol === 'acp', not on a backend id")
	}
}
