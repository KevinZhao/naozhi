package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// The runtime semantics of a denied flag in args are: naozhi STARTS, the flag is
// reported as a spawn diag, and the spawn pipeline strips it. That combination —
// not a load failure, not a silent drop — is the whole design (#2493): refusing
// to boot over a stale `--append-system-prompt` would take a deployment down for
// a value the dedicated field replaces, and dropping it silently is what cost
// three months of a stripped `--effort` (#2412). Nothing in this package tested
// it, so either half could flip without a red test: a `return fmt.Errorf` in
// validateArgvStrings turns every such config fatal, and deleting the
// EmitSpawnDiags call turns the drop invisible again.

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// loadWithDiags loads path while collecting the diags Load emits.
func loadWithDiags(t *testing.T, path string) (*Config, []cli.SpawnDiag, error) {
	t.Helper()
	var mu sync.Mutex
	var diags []cli.SpawnDiag
	restore := cli.ObserveSpawnDiags(func(_ string, d cli.SpawnDiag) {
		mu.Lock()
		diags = append(diags, d)
		mu.Unlock()
	})
	cfg, err := Load(path)
	restore()
	mu.Lock()
	defer mu.Unlock()
	return cfg, diags, err
}

func diagFor(diags []cli.SpawnDiag, key string) (cli.SpawnDiag, bool) {
	for _, d := range diags {
		if d.Key == key {
			return d, true
		}
	}
	return cli.SpawnDiag{}, false
}

func TestDeniedFlagInArgs_LoadsAndReportsRatherThanRefusing(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		flag  string
		field string
	}{
		{
			name: "cli.args",
			body: `
cli:
  path: /usr/bin/true
  args: ["--mcp-config", "/tmp/x.json"]
`,
			flag:  "--mcp-config",
			field: "cli.args",
		},
		{
			name: "cli.backends[].args",
			body: `
cli:
  backends:
    - id: claude
      path: /usr/bin/true
      args: ["--effort", "high"]
`,
			flag:  "--effort",
			field: "cli.backends[claude].args",
		},
		{
			name: "agents[].args",
			body: `
cli:
  path: /usr/bin/true
agents:
  reviewer:
    args: ["--allowed-tools", "Bash"]
`,
			flag:  "--allowed-tools",
			field: "agents[reviewer].args",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, diags, err := loadWithDiags(t, writeConfigFile(t, tc.body))
			// Half one: it loads. A denied flag is an ineffective config, not a
			// broken one; naozhi must still start.
			if err != nil {
				t.Fatalf("Load must not fail over a denied flag in %s: %v", tc.field, err)
			}
			if cfg == nil {
				t.Fatal("Load returned no config")
			}
			// Half two: it is reported, naming the field so the operator can move
			// the value to its dedicated key.
			d, ok := diagFor(diags, tc.flag)
			if !ok {
				t.Fatalf("no diag for %s in %s; the strip would be invisible again. diags=%+v", tc.flag, tc.field, diags)
			}
			if d.Layer != "argv-denylist" || d.Action != "dropped" {
				t.Errorf("diag = layer %q action %q, want argv-denylist/dropped", d.Layer, d.Action)
			}
			if !strings.Contains(d.Reason, tc.field) {
				t.Errorf("reason %q must name the field so the operator can find it", d.Reason)
			}
			if !strings.Contains(d.Reason, "NOT reach the CLI") {
				t.Errorf("reason %q must say the flag does not reach the CLI", d.Reason)
			}
		})
	}
}

// The other half of the same validator DOES refuse: an empty element or a
// control byte is a broken config, not an ineffective one. Pinned alongside so a
// future edit cannot collapse the two verdicts into one.
func TestArgvStrings_EmptyAndControlBytesStillRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			name: "empty element",
			body: `
cli:
  path: /usr/bin/true
  args: ["--model", ""]
`,
			want: "is empty",
		},
		{
			name: "control byte",
			body: "\ncli:\n  path: /usr/bin/true\n  args: [\"--model\\u0001x\"]\n",
			want: "control byte",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := loadWithDiags(t, writeConfigFile(t, tc.body))
			if err == nil {
				t.Fatalf("Load must refuse %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must explain the refusal (%q)", err, tc.want)
			}
		})
	}
}
