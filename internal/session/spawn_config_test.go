package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestNewRouter_SpawnCarriesTheConfiguredSpawnSettings pins the path from
// RouterConfig through the spawn facet into a real spawn's options: both
// timeouts and both router-owned argv paths reach the SpawnOptions the CLI is
// started with. Values differ from every default so a dropped hop shows.
func TestNewRouter_SpawnCarriesTheConfiguredSpawnSettings(t *testing.T) {
	dir := t.TempDir()
	mcp, settings := filepath.Join(dir, "mcp.json"), filepath.Join(dir, "settings.json")
	r := NewRouter(RouterConfig{
		Wrapper:            cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude"),
		NoOutputTimeout:    3 * time.Minute,
		TotalTimeout:       7 * time.Minute,
		MCPConfigFile:      mcp,
		NaozhiSettingsFile: settings,
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
	if got.MCPConfigFile != mcp || got.SettingsFile != settings {
		t.Errorf("spawn paths = %q / %q, want %q / %q", got.MCPConfigFile, got.SettingsFile, mcp, settings)
	}
}
