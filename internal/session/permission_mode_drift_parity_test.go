package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/backendstore"
	"github.com/naozhi/naozhi/internal/shim"
)

// permissionModeRouter is a claude router on backend defaults whose spawn
// facet carries mode.
func permissionModeRouter(mode cli.PermissionMode) *Router {
	r := &Router{ss: newSessionTable()}
	r.editBackendsForTest(func(c *backendstore.Config) { c.Model = "opus" })
	r.spawn.permissionMode = mode
	return r
}

// TestPermissionModeSwitch_ReadsAsDrift: switching cli.permission_mode is a
// real argv change, so a shim spawned under the old mode is drifted (respawned
// once) after the restart, and staying on one mode is not.
func TestPermissionModeSwitch_ReadsAsDrift(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:alice:general"
	w := cli.NewWrapperLazy("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
	modes := []cli.PermissionMode{cli.PermissionModeDefault, cli.PermissionModeAuto}
	for _, spawnedUnder := range modes {
		spawner := permissionModeRouter(spawnedUnder)
		stored := driftArgsFor(spawner).driftCompareArgs(w, "claude", key, nil, &shim.SpawnOverlay{})
		for _, now := range modes {
			r := permissionModeRouter(now)
			state := shim.State{Key: key, CLIArgs: stored, SpawnOverlay: &shim.SpawnOverlay{}}
			drift, _, _ := driftArgsFor(r).shimArgsDrift(w, "claude", state, nil)
			if want := spawnedUnder != now; drift != want {
				t.Errorf("spawned under %d, restarted under %d: drift = %v, want %v", spawnedUnder, now, drift, want)
			}
		}
	}
}
