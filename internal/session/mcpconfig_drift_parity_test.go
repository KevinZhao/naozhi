package session

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestSpawnArgvConstructor_CarriesMCPConfigFile is the RFC cli-mcp-config half
// of the parity pair (the full field list lives in
// TestEffortDriftCheck_MirrorsSpawn).
//
// Source-level rather than behavioural for the same reason its Effort twin is:
// deleting `MCPConfigFile: r.mcpConfigFile` from argvSpawnOptions compiles and
// passes every behavioural test — the configured MCP set just silently stops
// reaching the CLI, and the operator sees "cli.mcp_config does nothing" with no
// failing test anywhere.
func TestSpawnArgvConstructor_CarriesMCPConfigFile(t *testing.T) {
	t.Parallel()
	got, literals := spawnOptionsLiteralFields(t, argvConstructorFile)
	if literals == 0 {
		t.Fatalf("no cli.SpawnOptions literal found in %s — if the argv "+
			"constructor moved, move this test with it", argvConstructorFile)
	}
	if !slices.Contains(got, "MCPConfigFile") {
		t.Errorf("argvSpawnOptions does not set MCPConfigFile (sets %v) — a configured "+
			"cli.mcp_config would never reach the CLI, and no behavioural test "+
			"would notice", got)
	}
}

// TestMCPConfigFileAffectsArgv pins the premise both parity assertions rest on:
// MCPConfigFile must actually change the argv. If it stopped doing so, the
// mirror in router_shim.go would be dead weight and its comment misleading.
func TestMCPConfigFileAffectsArgv(t *testing.T) {
	t.Parallel()
	proto := &cli.ClaudeProtocol{}
	with := proto.BuildArgs(cli.SpawnOptions{Model: "opus", MCPConfigFile: "/data/mcp.json"})
	without := proto.BuildArgs(cli.SpawnOptions{Model: "opus"})

	if slices.Equal(with, without) {
		t.Fatal("ClaudeProtocol.BuildArgs ignores MCPConfigFile — the configured MCP " +
			"servers no longer reach cc, and the drift-check mirror in router_shim.go " +
			"is now pointless")
	}
	if !slices.Contains(with, "--mcp-config") {
		t.Errorf("expected --mcp-config in argv, got %v", with)
	}
}

// TestMCPConfigDriftParity_NoFalsePositive is the behavioural complement to the
// AST assertions: with cli.mcp_config configured, the argv the drift check
// builds must EQUAL the argv the real spawn builds, so a surviving shim is not
// misread as arg-drift.
//
// This is the failure the AST tests exist to prevent, asserted end-to-end on the
// values rather than on the source: without the mirror, every live session gets
// restarted the first time naozhi restarts after the operator turns
// cli.mcp_config on. RFC cli-mcp-config G4.
//
// Both sides run the PRODUCTION constructor. They used to be two identical
// hand-written SpawnOptions literals, which made this assertion vacuous — it
// compared the test's own copies, so no production divergence could ever fail it
// (the DebugFile regression lived through it untouched).
func TestMCPConfigDriftParity_NoFalsePositive(t *testing.T) {
	t.Parallel()
	const mcpPath = "/data/naozhi/mcp.json"

	key := "dashboard:direct:mcp-parity:general"
	r := &Router{}
	r.bkStore.model = "opus"
	r.mcpConfigFile = mcpPath
	proto := &cli.ClaudeProtocol{}

	bd := r.backendDefaultsFor("claude")

	// What classifyShimState's drift check builds (driftCompareArgs → read-only
	// debug path).
	driftArgs := proto.BuildArgs(
		r.argvSpawnOptions(bd.Model, bd.Effort, r.cliDebugPathFor(key), bd.Args))
	// What spawnSession builds for a session on backend defaults
	// (router_lifecycle.go → side-effecting debug path).
	spawnArgs := proto.BuildArgs(
		r.argvSpawnOptions(bd.Model, bd.Effort, r.cliDebugFileFor(key), bd.Args))

	if !slices.Equal(stripResumeArgs(spawnArgs), stripResumeArgs(driftArgs)) {
		t.Errorf("drift check disagrees with the real spawn:\n spawn = %v\n drift = %v",
			spawnArgs, driftArgs)
	}
	if !slices.Contains(driftArgs, mcpPath) {
		t.Errorf("the configured MCP path never reached the drift argv: %v", driftArgs)
	}
}
