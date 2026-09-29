package config

import (
	"testing"
)

// TestLoad_SessionCWDWorkspaceReconcile pins #1782 through the migration
// chain: session.cwd and the deprecated session.workspace are reconciled on
// the operator's RAW input, so a pure-default deployment reports nothing, and
// the default lands in cwd only when neither key is set.
func TestLoad_SessionCWDWorkspaceReconcile(t *testing.T) {
	tests := []struct {
		name       string
		session    string
		wantAction string // the session.workspace diag's action; "" = none
		wantCWD    string
	}{
		{name: "pure_default_no_diag", wantCWD: defaultSessionCWD},
		{name: "only_cwd_no_diag", session: "  cwd: /srv/work\n", wantCWD: "/srv/work"},
		{name: "only_workspace_rewritten", session: "  workspace: /srv/legacy\n", wantAction: "rewritten", wantCWD: "/srv/legacy"},
		{name: "both_set_cwd_wins", session: "  cwd: /srv/work\n  workspace: /srv/legacy\n", wantAction: "ignored", wantCWD: "/srv/work"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "platforms:\n  weixin:\n    token: \"x\"\n"
			if tt.session != "" {
				body += "session:\n" + tt.session
			}
			diags, cfg, err := collectLoadDiags(t, writeCfg(t, body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var actions []string
			for _, d := range diags {
				if d.Key == "session.workspace" {
					actions = append(actions, d.Action)
				}
				if d.Layer == "config-unknown" {
					t.Errorf("unexpected unknown-key diag %+v", d)
				}
			}
			switch {
			case tt.wantAction == "" && len(actions) != 0:
				t.Errorf("session.workspace diags %v, want none", actions)
			case tt.wantAction != "" && (len(actions) != 1 || actions[0] != tt.wantAction):
				t.Errorf("session.workspace diags %v, want exactly [%s]", actions, tt.wantAction)
			}
			if cfg.Session.CWD != tt.wantCWD {
				t.Errorf("session.cwd = %q, want %q", cfg.Session.CWD, tt.wantCWD)
			}
		})
	}
}
