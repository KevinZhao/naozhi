package cli

import (
	"slices"
	"testing"
)

// mcpConfigArgs extracts every --mcp-config value from a BuildArgs result.
// Returns a slice (not a single string) so tests can assert the flag is emitted
// EXACTLY once — a duplicate would make cc read a different file than intended.
func mcpConfigArgs(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--mcp-config" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// TestClaudeProtocol_BuildArgs_MCPConfigDefault locks RFC cli-mcp-config G2:
// a zero-value MCPConfigFile emits no --mcp-config at all, so every existing
// spawn stays argv-identical after this feature lands.
func TestClaudeProtocol_BuildArgs_MCPConfigDefault(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	args := p.BuildArgs(SpawnOptions{Model: "opus"}) // MCPConfigFile zero value
	if got := mcpConfigArgs(args); len(got) != 0 {
		t.Errorf("default: unexpected --mcp-config %v; args=%v", got, args)
	}
}

// TestClaudeProtocol_BuildArgs_MCPConfigAbs locks the happy path: an absolute
// path is rendered exactly once as `--mcp-config <path>`.
func TestClaudeProtocol_BuildArgs_MCPConfigAbs(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	path := "/data/naozhi/mcp.json"
	args := p.BuildArgs(SpawnOptions{Model: "opus", MCPConfigFile: path})
	got := mcpConfigArgs(args)
	if len(got) != 1 {
		t.Fatalf("want exactly one --mcp-config, got %v; args=%v", got, args)
	}
	if got[0] != path {
		t.Errorf("--mcp-config = %q, want %q; args=%v", got[0], path, args)
	}
}

// TestClaudeProtocol_BuildArgs_MCPConfigGuards locks the argv-injection /
// path-safety guards, mirroring TestClaudeProtocol_BuildArgs_SettingsFileGuards:
// a relative path or one starting with '-' must be dropped rather than handed to
// the CLI, where it could be reinterpreted as another flag or resolve against
// the session workspace.
func TestClaudeProtocol_BuildArgs_MCPConfigGuards(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	for _, bad := range []string{
		"mcp.json",        // bare filename (relative)
		"./mcp.json",      // cwd-relative
		"../x/mcp.json",   // parent-relative
		"-rf",             // leading dash (flag injection)
		"-/abs/looking",   // leading dash even if slashy
		"--mcp-config=/x", // flag-shaped
	} {
		args := p.BuildArgs(SpawnOptions{Model: "opus", MCPConfigFile: bad})
		if got := mcpConfigArgs(args); len(got) != 0 {
			t.Errorf("bad MCPConfigFile %q: must not emit --mcp-config, got %v; args=%v", bad, got, args)
		}
		// A dropped value must not slide into argv as a bare token either.
		if slices.Contains(args, bad) {
			t.Errorf("bad MCPConfigFile %q leaked into argv as a free-standing token; args=%v", bad, args)
		}
	}
}

// TestClaudeProtocol_BuildArgs_MCPConfigIndependentOfSettings locks RFC
// cli-mcp-config §7: MCPConfigFile and SettingsFile are orthogonal knobs. All
// four combinations are legal and the presence of --mcp-config is decided by
// MCPConfigFile alone, never by which settings branch ran.
func TestClaudeProtocol_BuildArgs_MCPConfigIndependentOfSettings(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	const mcp = "/data/naozhi/mcp.json"
	const settings = "/data/naozhi/naozhi-settings.json"

	for _, tc := range []struct {
		name       string
		settings   string
		mcp        string
		wantMCP    bool
		wantSource string
	}{
		{"neither", "", "", false, "user"},
		{"settings only", settings, "", false, ""},
		{"mcp only", "", mcp, true, "user"},
		{"both", settings, mcp, true, ""},
	} {
		args := p.BuildArgs(SpawnOptions{
			Model:         "opus",
			SettingsFile:  tc.settings,
			MCPConfigFile: tc.mcp,
		})
		gotMCP := mcpConfigArgs(args)
		if got := len(gotMCP) == 1; got != tc.wantMCP {
			t.Errorf("%s: --mcp-config present = %v, want %v; args=%v", tc.name, got, tc.wantMCP, args)
		}
		if tc.wantMCP && len(gotMCP) == 1 && gotMCP[0] != mcp {
			t.Errorf("%s: --mcp-config = %q, want %q", tc.name, gotMCP[0], mcp)
		}
		// The settings branch must be unaffected by MCPConfigFile.
		if source, _ := settingsArgs(t, args); source != tc.wantSource {
			t.Errorf("%s: --setting-sources = %q, want %q; args=%v", tc.name, source, tc.wantSource, args)
		}
	}
}

// TestBuildArgs_MCPConfigStillDeniedInExtraArgs locks RFC cli-mcp-config NG1:
// adding the dedicated MCPConfigFile field must NOT relax the deniedExtraFlags
// entry. `--mcp-config` smuggled through ExtraArgs (agent args, project config,
// a prompt-influenced path) still gets stripped — the dedicated field is the
// only sanctioned injection point, because only it comes from operator config.
func TestBuildArgs_MCPConfigStillDeniedInExtraArgs(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	for _, extra := range [][]string{
		{"--mcp-config", "/tmp/evil.json"},
		{"--mcp-config=/tmp/evil.json"},
	} {
		args := p.BuildArgs(SpawnOptions{Model: "opus", ExtraArgs: extra})
		if got := mcpConfigArgs(args); len(got) != 0 {
			t.Errorf("ExtraArgs %v: --mcp-config must be stripped, got %v; args=%v", extra, got, args)
		}
		if slices.Contains(args, "/tmp/evil.json") {
			t.Errorf("ExtraArgs %v: orphaned value leaked into argv; args=%v", extra, args)
		}
		if slices.Contains(args, "--mcp-config=/tmp/evil.json") {
			t.Errorf("ExtraArgs %v: equals-form leaked into argv; args=%v", extra, args)
		}
	}
}
