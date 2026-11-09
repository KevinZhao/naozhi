package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cliinfo"
)

// TestNewRouter_SpawnCarriesTheConfiguredSpawnSettings pins the path from
// RouterConfig through the spawn facet into a real spawn's options: both
// timeouts, both router-owned argv paths and the permission mode reach the
// SpawnOptions the CLI is started with. Values differ from every default so a dropped hop shows.
func TestNewRouter_SpawnCarriesTheConfiguredSpawnSettings(t *testing.T) {
	dir := t.TempDir()
	mcp, settings := filepath.Join(dir, "mcp.json"), filepath.Join(dir, "settings.json")
	r := NewRouter(RouterConfig{
		Wrapper:            cli.NewWrapperLazy("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude"),
		NoOutputTimeout:    3 * time.Minute,
		TotalTimeout:       7 * time.Minute,
		MCPConfigFile:      mcp,
		NaozhiSettingsFile: settings,
		PermissionMode:     cli.PermissionModeAuto,
	})
	t.Cleanup(r.Shutdown)
	var got cli.SpawnOptions
	r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		got = opts
		return newIdleProc(), nil
	}

	if _, _, err := r.GetOrCreate(context.Background(), "feishu:direct:alice:general", AgentOpts{}); err != nil {
		t.Fatal(err)
	}

	if got.NoOutputTimeout != 3*time.Minute || got.TotalTimeout != 7*time.Minute {
		t.Errorf("spawn timeouts = %v / %v, want 3m / 7m", got.NoOutputTimeout, got.TotalTimeout)
	}
	if _, _, err := r.GetOrCreate(context.Background(), "task:spawn-budget", AgentOpts{}); err != nil {
		t.Fatal(err)
	}
	if got.TotalTimeout != cliinfo.DefaultTaskTotalTimeout {
		t.Errorf("task: spawn TotalTimeout = %v, want the task floor %v over the configured 7m", got.TotalTimeout, cliinfo.DefaultTaskTotalTimeout)
	}
	if got.MCPConfigFile != mcp || got.SettingsFile != settings {
		t.Errorf("spawn paths = %q / %q, want %q / %q", got.MCPConfigFile, got.SettingsFile, mcp, settings)
	}
	if got.PermissionMode != cli.PermissionModeAuto {
		t.Errorf("spawn PermissionMode = %d, want PermissionModeAuto", got.PermissionMode)
	}
}

// TestTotalTimeoutFor pins the per-key budget: a task: session gets at least
// the task floor and keeps a longer configured one; every other key gets the
// configured value untouched (0 = the process default).
func TestTotalTimeoutFor(t *testing.T) {
	for _, tc := range []struct {
		key        string
		configured time.Duration
		want       time.Duration
	}{
		{"feishu:direct:alice:general", 0, 0},
		{"feishu:direct:alice:general", 7 * time.Minute, 7 * time.Minute},
		{"dashboard:direct:x:general", 8 * time.Hour, 8 * time.Hour},
		{"task:abc", 0, cliinfo.DefaultTaskTotalTimeout},
		{"task:abc", 7 * time.Minute, cliinfo.DefaultTaskTotalTimeout},
		{"task:abc", 8 * time.Hour, 8 * time.Hour},
		{"cron:task:abc", 0, 0},
	} {
		c := spawnConfig{totalTimeout: tc.configured}
		if got := c.totalTimeoutFor(tc.key); got != tc.want {
			t.Errorf("totalTimeoutFor(%q) with %v configured = %v, want %v", tc.key, tc.configured, got, tc.want)
		}
	}
}
