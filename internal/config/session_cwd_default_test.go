package config

import (
	"strings"
	"testing"
)

// TestApplyDefaults_SessionCWDWorkspaceReconcile pins #1782: applyDefaults must
// reconcile session.cwd / deprecated session.workspace against the operator's
// RAW input before filling the default. Filling the default into the deprecated
// workspace field first (the old bug) made a pure-default deployment falsely
// trip the deprecation warning.
//
// The four cases below cover:
//
//	(a) pure default      -> no warning at all, default lands in cwd
//	(b) only cwd set       -> no "both ... configured" warning
//	(c) only workspace set -> deprecation warning still fires
//	(d) default applied    -> final cwd is non-empty
func TestApplyDefaults_SessionCWDWorkspaceReconcile(t *testing.T) {
	const deprecMsg = "'session.workspace' is deprecated"
	const bothMsg = "both 'session.cwd' and deprecated 'session.workspace'"

	tests := []struct {
		name           string
		cwd            string
		workspace      string
		wantDeprecWarn bool
		wantBothWarn   bool
		wantCWD        string
	}{
		{
			name:           "pure_default_no_warning",
			wantDeprecWarn: false,
			wantBothWarn:   false,
			wantCWD:        defaultSessionCWD,
		},
		{
			name:           "only_cwd_no_both_warning",
			cwd:            "/srv/work",
			wantDeprecWarn: false,
			wantBothWarn:   false,
			wantCWD:        "/srv/work",
		},
		{
			name:           "only_workspace_warns_deprecation",
			workspace:      "/srv/legacy",
			wantDeprecWarn: true,
			wantBothWarn:   false,
			wantCWD:        "/srv/legacy",
		},
		{
			name:           "both_set_diverging_warns_both",
			cwd:            "/srv/work",
			workspace:      "/srv/legacy",
			wantDeprecWarn: false,
			wantBothWarn:   true,
			wantCWD:        "/srv/work",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Session.CWD = tt.cwd
			cfg.Session.Workspace = tt.workspace

			out := captureSlog(t, func() {
				applyDefaults(cfg)
			})

			if got := strings.Contains(out, deprecMsg); got != tt.wantDeprecWarn {
				t.Errorf("deprecation warn = %v, want %v; log = %q", got, tt.wantDeprecWarn, out)
			}
			if got := strings.Contains(out, bothMsg); got != tt.wantBothWarn {
				t.Errorf("both warn = %v, want %v; log = %q", got, tt.wantBothWarn, out)
			}
			if cfg.Session.CWD == "" {
				t.Errorf("session.cwd must be non-empty after defaults; got empty")
			}
			if cfg.Session.CWD != tt.wantCWD {
				t.Errorf("session.cwd = %q, want %q", cfg.Session.CWD, tt.wantCWD)
			}
			// The deprecated alias must stay mirrored so existing readers work.
			if cfg.Session.Workspace != cfg.Session.CWD {
				t.Errorf("session.workspace = %q, want mirror of cwd %q",
					cfg.Session.Workspace, cfg.Session.CWD)
			}
		})
	}
}
