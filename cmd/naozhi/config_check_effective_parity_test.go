package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// `--effective` claims to print "what the real spawn passes". It kept its own
// copy of the argv precedence and drifted from resolveSpawnParams on the three
// most common shapes (#2969): agent args, the default access profile's
// default_model, and a profile that injects a credential from a *_FILE. These
// tests pin each against the runtime rule (session.mergeArgvLayers via
// session.EffectiveArgvLayers, and session.resolveEnvOverlay's semantics).
func TestConfigCheckEffective_AgentArgsAppendToBackendArgs(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  path: /usr/bin/true
  model: sonnet
  args: ["--max-budget-usd", "5"]
agents:
  rev:
    args: ["--max-turns", "3"]
`
	got, raw := effectiveOf(t, cfg)
	argv, ok := got.Effective["claude"].Agents["rev"]
	if !ok {
		t.Fatalf("no argv for agent rev:\n%s", raw)
	}
	// Runtime: args = backend.Args ++ agent.Args, never a replacement.
	if !argvPair(argv, "--max-budget-usd", "5") {
		t.Errorf("agent argv lost the backend args (reported as replace, runtime appends): %v", argv)
	}
	if !argvPair(argv, "--max-turns", "3") {
		t.Errorf("agent argv lost the agent's own args: %v", argv)
	}
	bi := slices.Index(argv, "--max-budget-usd")
	ai := slices.Index(argv, "--max-turns")
	if bi > ai {
		t.Errorf("backend args must precede agent args, as mergeArgvLayers appends them: %v", argv)
	}
}

func TestConfigCheckEffective_DefaultAccessProfileModelWins(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  path: /usr/bin/true
  model: sonnet
agents:
  rev:
    args: ["--max-turns", "3"]
  pinned:
    model: haiku
access_profiles:
  work:
    default_model: opus
default_access_profile: work
`
	got, raw := effectiveOf(t, cfg)
	eff, ok := got.Effective["claude"]
	if !ok {
		t.Fatalf("no effective entry for claude:\n%s", raw)
	}
	// Chain: backend.Model < profile default_model < agent.Model.
	if !argvPair(eff.Argv, "--model", "opus") {
		t.Errorf("base argv ignores default_access_profile.default_model: %v", eff.Argv)
	}
	if !argvPair(eff.Agents["rev"], "--model", "opus") {
		t.Errorf("agent without a model must inherit the profile default: %v", eff.Agents["rev"])
	}
	if !argvPair(eff.Agents["pinned"], "--model", "haiku") {
		t.Errorf("an agent's own model outranks the profile default: %v", eff.Agents["pinned"])
	}
}

func TestConfigCheckEffective_ProfileFileSecretIsShownNotDropped(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte("sk-ant-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cleanCheckConfig + `
cli:
  path: /usr/bin/true
access_profiles:
  work:
    env:
      ANTHROPIC_AUTH_TOKEN_FILE: "` + tok + `"
      AWS_REGION: "us-west-2"
`
	got, raw := effectiveOf(t, cfg)
	work, ok := got.Effective["claude"].Profiles["work"]
	if !ok {
		t.Fatalf("no env reported for profile work:\n%s", raw)
	}
	var tokenEntry string
	for _, kv := range work {
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			tokenEntry = kv
		}
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN_FILE=") {
			t.Errorf("the *_FILE key itself is never forwarded by the runtime: %q", kv)
		}
	}
	// Runtime reads the file and injects the CONCRETE key; the report names
	// the source without reading the secret.
	if tokenEntry == "" {
		t.Fatalf("profile env shows no ANTHROPIC_AUTH_TOKEN; the runtime injects one from %s:\n%v", tok, work)
	}
	if !strings.Contains(tokenEntry, tok) {
		t.Errorf("entry must name the file it is read from: %q", tokenEntry)
	}
	if strings.Contains(raw, "sk-ant") {
		t.Errorf("the secret's bytes leaked into the report:\n%s", raw)
	}
	if !slices.Contains(work, "AWS_REGION=us-west-2") {
		t.Errorf("literal overlay entries still apply: %v", work)
	}
	for _, d := range got.Diags {
		if strings.Contains(d.Key, "ANTHROPIC_AUTH_TOKEN") {
			t.Errorf("a readable secret file is not a diag: %+v", d)
		}
	}
}

func TestConfigCheckEffective_MissingProfileFileIsADiag(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  path: /usr/bin/true
access_profiles:
  work:
    env:
      ANTHROPIC_AUTH_TOKEN_FILE: "/nonexistent/naozhi-check/tok"
`
	got, raw := effectiveOf(t, cfg)
	found := false
	for _, d := range got.Diags {
		if d.Layer == "access-profile" && strings.Contains(d.Key, "access_profiles.work.env.ANTHROPIC_AUTH_TOKEN_FILE") {
			found = true
			if !strings.Contains(d.Reason, "fails") {
				t.Errorf("reason must say the spawn fails loudly: %q", d.Reason)
			}
		}
	}
	if !found {
		t.Errorf("an unreadable *_FILE makes every spawn under the profile FAIL; check reported OK:\n%s", raw)
	}
	for _, kv := range got.Effective["claude"].Profiles["work"] {
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN") {
			t.Errorf("no credential can be injected from a missing file: %q", kv)
		}
	}
}
