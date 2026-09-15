package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// `--effective` used to build its own three-field SpawnOptions
// ({Model, Effort, ExtraArgs}), so the argv it printed was missing every field
// the runtime fills in at startup: the report said `--setting-sources user`
// while the real spawn passed `--setting-sources ""` plus `--settings <path>`,
// and dropped `--mcp-config` entirely. An operator checking whether sessions
// still read ~/.claude/settings.json got the wrong answer from the one command
// built to answer it. It now goes through session.ArgvSpawnOptions, the same
// builder the spawn and the drift comparison use.

// effectiveOf runs `config check -effective -json` and returns the decoded
// result plus the raw output for failure messages.
func effectiveOf(t *testing.T, cfg string) (checkResult, string) {
	t.Helper()
	var out bytes.Buffer
	configCheck([]string{"-config", writeCheckConfig(t, cfg), "-effective", "-json"}, &out)
	var got checkResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode -json output: %v\n%s", err, out.String())
	}
	return got, out.String()
}

// argvPair reports whether argv contains flag immediately followed by value.
func argvPair(argv []string, flag, value string) bool {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
	}
	return false
}

func TestConfigCheckEffective_ReportsTheStartupResolvedPaths(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "naozhi-settings.json")
	// The file has to exist: resolveMCPConfigFile stats and parses it, and
	// returns "" on any problem, because cc refuses to start on a bad
	// --mcp-config. The report follows that resolution, not the raw config value.
	mcp := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}
	cfg := cleanCheckConfig + `
cli:
  path: /usr/bin/true
  model: sonnet
  mcp_config: "` + mcp + `"
naozhi_settings:
  enabled: true
  path: "` + settings + `"
`
	got, raw := effectiveOf(t, cfg)
	eff, ok := got.Effective["claude"]
	if !ok {
		t.Fatalf("no effective entry for claude:\n%s", raw)
	}
	if !argvPair(eff.Argv, "--settings", settings) {
		t.Errorf("argv must carry the resolved naozhi settings file: %v", eff.Argv)
	}
	if !argvPair(eff.Argv, "--mcp-config", mcp) {
		t.Errorf("argv must carry the resolved mcp config: %v", eff.Argv)
	}
	// The isolated-settings path suppresses external sources; reporting `user`
	// here is the misread this test exists to prevent.
	if !argvPair(eff.Argv, "--setting-sources", "") {
		t.Errorf("argv must show settings sources suppressed, got %v", eff.Argv)
	}
}

func TestConfigCheckEffective_DropsAnEffortTheBackendCannotTake(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  effort: high
  backends:
    - id: claude
      path: /usr/bin/true
    - id: codex
      path: /usr/bin/true
`
	got, raw := effectiveOf(t, cfg)
	claude, ok := got.Effective["claude"]
	if !ok {
		t.Fatalf("no effective entry for claude:\n%s", raw)
	}
	if !argvPair(claude.Argv, "--effort", "high") {
		t.Errorf("claude accepts a tier, so argv must carry it: %v", claude.Argv)
	}
	codex, ok := got.Effective["codex"]
	if !ok {
		t.Fatalf("no effective entry for codex:\n%s", raw)
	}
	// The startup path drops the tier for a backend without the capability, so
	// an argv that shows it describes a spawn that never happens.
	if slices.Contains(codex.Argv, "--effort") {
		t.Errorf("codex takes no thinking-effort tier; argv must not show one: %v", codex.Argv)
	}
	// And the drop is still reported rather than silently applied.
	found := false
	for _, d := range got.Diags {
		if d.Backend == "codex" && d.Layer == "caps" && strings.Contains(d.Key, "effort") {
			found = true
		}
	}
	if !found {
		t.Errorf("dropping the tier must be reported as a caps diag:\n%s", raw)
	}
}

func TestConfigCheckEffective_ReportsAgentsAndAccessProfiles(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  path: /usr/bin/true
agents:
  reviewer:
    system_prompt: "You review code."
access_profiles:
  prod:
    env:
      AWS_REGION: "us-west-2"
`
	got, raw := effectiveOf(t, cfg)
	eff, ok := got.Effective["claude"]
	if !ok {
		t.Fatalf("no effective entry for claude:\n%s", raw)
	}
	// An agent's system_prompt is session-scoped, so it cannot be in the base
	// argv — but leaving it out of the report entirely is what made the
	// "effective" argv unfaithful for every agent session.
	if slices.Contains(eff.Argv, "--append-system-prompt") {
		t.Errorf("base argv is the no-agent case; it must not carry an agent prompt: %v", eff.Argv)
	}
	agentArgv, ok := eff.Agents["reviewer"]
	if !ok {
		t.Fatalf("no argv reported for agent reviewer:\n%s", raw)
	}
	if !argvPair(agentArgv, "--append-system-prompt", "You review code.") {
		t.Errorf("agent argv must carry its system prompt: %v", agentArgv)
	}
	prod, ok := eff.Profiles["prod"]
	if !ok {
		t.Fatalf("no env reported for access profile prod:\n%s", raw)
	}
	if !slices.Contains(prod, "AWS_REGION=us-west-2") {
		t.Errorf("profile env must show the overlay applied: %v", prod)
	}
}

func TestConfigCheckEffective_NonAllowlistedOverlayKeyIsFatal(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  path: /usr/bin/true
access_profiles:
  prod:
    env:
      NOT_ALLOWED_KEY: "x"
      AWS_REGION: "eu-west-1"
`
	var out bytes.Buffer
	code := configCheck([]string{"-config", writeCheckConfig(t, cfg), "-effective", "-json"}, &out)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (fatal); output:\n%s", code, out.String())
	}
	// The overlay allowlist is enforced at load, not at report time — which is
	// why the reported profile env never has to show a filtered overlay.
	if !strings.Contains(out.String(), "NOT_ALLOWED_KEY") || !strings.Contains(out.String(), "overlay allowlist") {
		t.Errorf("the fatal must name the refused key and the allowlist:\n%s", out.String())
	}
}
