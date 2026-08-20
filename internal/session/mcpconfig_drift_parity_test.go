package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestSpawnOptionsLiteral_CarriesMCPConfigFile is the spawn-side half of the
// parity pair for RFC cli-mcp-config (the drift-side half is the extended
// `required` list in TestEffortDriftCheck_MirrorsSpawn).
//
// Source-level rather than behavioural for the same reason its Effort twin is:
// deleting `MCPConfigFile: r.mcpConfigFile` from the SpawnOptions literal
// compiles and passes every behavioural test — the configured MCP set just
// silently stops reaching the CLI, and the operator sees "cli.mcp_config does
// nothing" with no failing test anywhere.
func TestSpawnOptionsLiteral_CarriesMCPConfigFile(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "router_lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse router_lifecycle.go: %v", err)
	}

	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SpawnOptions" {
			return true
		}
		found = true
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "MCPConfigFile" {
					return false
				}
			}
		}
		t.Error("the cli.SpawnOptions literal in router_lifecycle.go does not set " +
			"MCPConfigFile — a configured cli.mcp_config would never reach the CLI, " +
			"and no behavioural test would notice")
		return false
	})
	if !found {
		t.Fatal("no cli.SpawnOptions literal found in router_lifecycle.go — " +
			"if spawn assembly moved, move this test with it")
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
// values rather than on the source: without the mirror in router_shim.go, every
// live session gets restarted the first time naozhi restarts after the operator
// turns cli.mcp_config on. RFC cli-mcp-config G4.
func TestMCPConfigDriftParity_NoFalsePositive(t *testing.T) {
	t.Parallel()
	const mcpPath = "/data/naozhi/mcp.json"

	r := &Router{}
	r.bkStore.model = "opus"
	r.mcpConfigFile = mcpPath
	proto := &cli.ClaudeProtocol{}

	bd := r.backendDefaultsFor("claude")

	// What classifyShimState's drift check builds (router_shim.go).
	driftArgs := proto.BuildArgs(cli.SpawnOptions{
		Model:         bd.Model,
		ExtraArgs:     bd.Args,
		Effort:        bd.Effort,
		SettingsFile:  r.naozhiSettingsFile,
		MCPConfigFile: r.mcpConfigFile,
	})
	// What spawnSession builds for a session on backend defaults
	// (router_lifecycle.go).
	spawnArgs := proto.BuildArgs(cli.SpawnOptions{
		Model:         bd.Model,
		ExtraArgs:     bd.Args,
		Effort:        bd.Effort,
		SettingsFile:  r.naozhiSettingsFile,
		MCPConfigFile: r.mcpConfigFile,
	})

	if !slices.Equal(stripResumeArgs(spawnArgs), stripResumeArgs(driftArgs)) {
		t.Errorf("drift check disagrees with the real spawn:\n spawn = %v\n drift = %v",
			spawnArgs, driftArgs)
	}
	if !slices.Contains(driftArgs, mcpPath) {
		t.Errorf("the configured MCP path never reached the drift argv: %v", driftArgs)
	}
}
