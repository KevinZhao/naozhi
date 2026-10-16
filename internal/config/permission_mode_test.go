package config

import (
	"strings"
	"testing"
)

// TestLoad_PermissionMode: cli.permission_mode accepts only "", "bypass" and
// "auto". The CLI's other modes (manual, acceptEdits, plan, bypassPermissions…)
// would stall or silently change a headless spawn, and a typo must not fall
// back to bypass without the operator noticing.
func TestLoad_PermissionMode(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", PermissionModeBypass, PermissionModeAuto} {
		cfg, err := Load(writeCfg(t, "cli:\n  permission_mode: \""+ok+"\"\n"))
		if err != nil {
			t.Errorf("Load(permission_mode %q) = %v, want nil", ok, err)
			continue
		}
		if cfg.CLI.PermissionMode != ok {
			t.Errorf("CLI.PermissionMode = %q, want %q", cfg.CLI.PermissionMode, ok)
		}
	}
	for _, bad := range []string{"standard", "Auto", "manual", "bypassPermissions", "acceptEdits", "plan", " auto"} {
		_, err := Load(writeCfg(t, "cli:\n  permission_mode: \""+bad+"\"\n"))
		if err == nil || !strings.Contains(err.Error(), "cli.permission_mode") {
			t.Errorf("Load(permission_mode %q) = %v, want a cli.permission_mode error", bad, err)
		}
	}
}
