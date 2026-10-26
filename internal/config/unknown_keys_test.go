package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/spawndiag"
)

// writeCfg writes body to a temp config.yaml and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// collectLoadDiags loads path with a diag observer installed and returns the
// diags Load emitted, the config and the error.
func collectLoadDiags(t *testing.T, path string) ([]spawndiag.Diag, *Config, error) {
	t.Helper()
	var got []spawndiag.Diag
	restore := spawndiag.Observe(func(scope string, d spawndiag.Diag) {
		if scope != "config" {
			t.Errorf("diag scope = %q, want \"config\"", scope)
		}
		got = append(got, d)
	})
	defer restore()
	cfg, err := Load(path)
	return got, cfg, err
}

func unknownDiags(in []spawndiag.Diag) []spawndiag.Diag {
	var out []spawndiag.Diag
	for _, d := range in {
		if d.Layer == "config-unknown" {
			out = append(out, d)
		}
	}
	return out
}

// TestUnknownKeysReported is the regression this file exists for: a misspelled
// or non-existent key used to load with no error, no effect and no diagnostic
// (#2639). The report names the dotted path, because the yaml decoder reports
// only the leaf and this schema reuses leaf names under many parents.
func TestUnknownKeysReported(t *testing.T) {
	body := "server:\n  debug_moed: true\ncli:\n  backends:\n    - id: claude\n      pahs: /usr/bin/claude\n"
	diags, cfg, err := collectLoadDiags(t, writeCfg(t, body))
	if err != nil || cfg == nil {
		t.Fatalf("Load must not fail on an unknown key: cfg=%v err=%v", cfg, err)
	}
	var paths []string
	for _, d := range unknownDiags(diags) {
		paths = append(paths, d.Key)
		if d.Action != "ignored" || !strings.Contains(d.Reason, "line ") {
			t.Errorf("diag %+v: want action \"ignored\" and a line number", d)
		}
	}
	if want := []string{"server.debug_moed", "cli.backends[0].pahs"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("unknown keys = %v, want %v", paths, want)
	}
}

// TestExampleConfigHasNoUnknownKeys checks the shipped config.example.yaml
// against the struct. An operator copies that file, so a key that drifted out
// of the struct while staying in the example is the #2553 shape with extra
// steps: documented, copied, ignored.
func TestExampleConfigHasNoUnknownKeys(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no config.example.yaml: %v", err)
	}
	if len(data) < 100 {
		t.Fatalf("config.example.yaml is %d bytes — a near-empty read would make this pass vacuously", len(data))
	}
	if got := findUnknownKeys(data); len(got) != 0 {
		for _, u := range got {
			t.Errorf("config.example.yaml line %d: key %q has no struct field", u.Line, u.Path)
		}
	}
}

// TestUnknownKeyDoesNotChangeLoadResult pins the "diagnostic only" contract: a
// config with an unknown key must decode to exactly the same Config as the same
// file without it. Reported, never acted on.
func TestUnknownKeyDoesNotChangeLoadResult(t *testing.T) {
	clean := "server:\n  dashboard_token: \"unknown-keys-test-token\"\nsession:\n  cwd: /tmp\n"
	dirty := clean + "server_typo_block:\n  nope: 1\n"

	a, err := Load(writeCfg(t, clean))
	if err != nil {
		t.Fatalf("Load clean: %v", err)
	}
	b, err := Load(writeCfg(t, dirty))
	if err != nil {
		t.Fatalf("Load dirty: %v", err)
	}
	// Fingerprint differs by construction (different bytes and path); everything
	// else must match.
	a.Fingerprint = Fingerprint{}
	b.Fingerprint = Fingerprint{}
	if !reflect.DeepEqual(a, b) {
		t.Error("an unknown key changed the loaded Config; it must be diagnostic only")
	}
}

// TestUnknownKeysNeverFatal covers the boot-safety promise: whatever the file
// contains, findUnknownKeys must not be the thing that stops naozhi starting.
// A stale key in a live config.yaml turning into "will not boot after upgrade"
// is worse than the silence this change removes.
func TestUnknownKeysNeverFatal(t *testing.T) {
	for _, body := range []string{
		"",
		"not_a_mapping\n",
		"- a\n- b\n",
		"anchor: &a\n  x: 1\nalias: *a\nbogus: 1\n",
		"server:\n  bogus: [1, 2, {deep: true}]\n",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("findUnknownKeys panicked on %q: %v", body, r)
				}
			}()
			findUnknownKeys([]byte(body))
		}()
	}
}

// TestDeprecatedKeysReported: the schema v1 spellings are no longer rewritten,
// so each must still load (a failing Load would take an upgraded service down)
// with exactly one diag naming the replacement, no duplicate config-unknown
// one, and no effect on the decoded config. The v1 --append-system-prompt in
// agents[].args gets a single argv-denylist diag, not a second
// config-deprecated one.
func TestDeprecatedKeysReported(t *testing.T) {
	diags, cfg, err := collectLoadDiags(t, writeCfg(t, `
nodes:
  mac:
    url: "https://mac.example.com"
server:
  metrics_enabled: true
  debug_mode: true
session:
  auto_chain:
    enabled: true
cron:
  sandbox:
    runtime_arn: "arn:aws:bedrock-agentcore:us-east-1:1:runtime/x"
    region: us-east-1
platforms:
  feishu:
    app_id: cli_x
    app_secret: s
    connection_mode: webhook
    allow_insecure_webhook: true
agents:
  reviewer:
    args: ["--append-system-prompt=Be terse.", "--keep"]
`))
	if err != nil {
		t.Fatalf("Load with v1 keys must not fail: %v", err)
	}
	want := map[string]string{
		"nodes":                  "workspaces",
		"session.auto_chain":     "remove",
		"server.metrics_enabled": "remove",
		"server.debug_mode":      "remove",
		"cron.sandbox":           "remove",

		"platforms.feishu.connection_mode":        "long connection",
		"platforms.feishu.allow_insecure_webhook": "remove",
	}
	flagDiags := 0
	for _, d := range diags {
		if strings.HasPrefix(d.Key, "--append-system-prompt") || strings.Contains(d.Key, "agents[reviewer]") {
			flagDiags++
			if d.Layer != "argv-denylist" || !strings.Contains(d.Reason, "agents[reviewer].system_prompt") {
				t.Errorf("--append-system-prompt diag %+v: want argv-denylist naming agents[reviewer].system_prompt", d)
			}
			continue
		}
		switch d.Layer {
		case "config-unknown":
			t.Errorf("unexpected config-unknown diag %+v", d)
		case "config-deprecated":
			hint, ok := want[d.Key]
			if !ok {
				t.Errorf("unexpected config-deprecated diag %+v", d)
				continue
			}
			if !strings.Contains(d.Reason, hint) {
				t.Errorf("%s reason %q does not name %q", d.Key, d.Reason, hint)
			}
			delete(want, d.Key)
		}
	}
	for k := range want {
		t.Errorf("no config-deprecated diag for %s", k)
	}
	if flagDiags != 1 {
		t.Errorf("--append-system-prompt in agents[].args drew %d diags, want exactly 1", flagDiags)
	}
	if len(cfg.Nodes) != 0 {
		t.Errorf("nodes must not be read any more, got %v", cfg.Nodes)
	}
	if got := cfg.Agents["reviewer"].SystemPrompt; got != "" {
		t.Errorf("--append-system-prompt must not be lifted any more, got %q", got)
	}
}
