package session

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/backendstore"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestSpawnArgvConstructor_CarriesPermissionMode is the cli.permission_mode
// half of the parity pair (the full field list lives in
// TestEffortDriftCheck_MirrorsSpawn). Dropping the field from argvSpawnOptions
// compiles and leaves every spawn on bypass, so the literal itself is asserted.
func TestSpawnArgvConstructor_CarriesPermissionMode(t *testing.T) {
	t.Parallel()
	got, literals := spawnOptionsLiteralFields(t, argvConstructorFile)
	if literals == 0 {
		t.Fatalf("no cli.SpawnOptions literal found in %s — if the argv "+
			"constructor moved, move this test with it", argvConstructorFile)
	}
	if !slices.Contains(got, "PermissionMode") {
		t.Errorf("argvSpawnOptions does not set PermissionMode (sets %v) — "+
			"cli.permission_mode: auto would never reach the CLI", got)
	}
}

// permissionModeRouter is a claude router on backend defaults whose spawn
// facet carries mode.
func permissionModeRouter(mode cli.PermissionMode) *Router {
	r := &Router{ss: newSessionTable()}
	r.editBackendsForTest(func(c *backendstore.Config) { c.Model = "opus" })
	r.spawn.permissionMode = mode
	return r
}

// TestPermissionModeDriftParity_NoFalsePositive: with auto mode configured,
// the argv the drift check rebuilds must equal the argv the real spawn built,
// or every surviving shim is killed on each restart.
func TestPermissionModeDriftParity_NoFalsePositive(t *testing.T) {
	t.Parallel()
	const key = "dashboard:direct:perm-parity:general"
	r := permissionModeRouter(cli.PermissionModeAuto)
	proto := &cli.ClaudeProtocol{}
	bd := r.backends.backendDefaultsFor("claude")

	driftArgs := proto.BuildArgs(
		r.spawn.argvSpawnOptions(bd.Model, bd.Effort, r.spawn.cliDebugPathFor(key), "", bd.Args))
	spawnArgs := proto.BuildArgs(
		r.spawn.argvSpawnOptions(bd.Model, bd.Effort, r.spawn.cliDebugFileFor(key), "", bd.Args))

	if !slices.Equal(stripResumeArgs(spawnArgs), stripResumeArgs(driftArgs)) {
		t.Errorf("drift check disagrees with the real spawn:\n spawn = %v\n drift = %v",
			spawnArgs, driftArgs)
	}
	if !slices.Contains(driftArgs, "--permission-mode") || slices.Contains(driftArgs, "--dangerously-skip-permissions") {
		t.Errorf("auto mode did not reach the drift argv: %v", driftArgs)
	}
}

// TestPermissionModeSwitch_ReadsAsDrift: switching cli.permission_mode is a
// real argv change, so a shim spawned under the old mode is drifted (respawned
// once) after the restart, and staying on one mode is not.
func TestPermissionModeSwitch_ReadsAsDrift(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:alice:general"
	w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
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
