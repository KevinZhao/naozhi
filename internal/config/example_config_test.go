package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cliinfo"
	"gopkg.in/yaml.v3"
)

// readExampleRoot parses the shipped config.example.yaml and returns its root
// mapping.
func readExampleRoot(t *testing.T) *yaml.Node {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config.example.yaml: %v", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		t.Fatal("config.example.yaml has no root mapping")
	}
	return doc.Content[0]
}

// TestExampleConfig_NeedsNoMigration: an operator copies the template, so any
// deprecated form left in it (the legacy agents[].args --append-system-prompt,
// `nodes:`, `session.workspace`) prints a config-deprecated WARN on first boot.
func TestExampleConfig_NeedsNoMigration(t *testing.T) {
	_, changes, err := runMigrations(readExampleRoot(t))
	if err != nil {
		t.Fatalf("runMigrations(config.example.yaml): %v", err)
	}
	for _, c := range changes {
		t.Errorf("config.example.yaml still uses a deprecated form: %s %s (%s)", c.Key, c.Action, c.Reason)
	}
}

// TestExampleConfig_TrustedProxyOff: a copied trusted_proxy: true refuses every
// direct LAN login with 400, so the template keeps the key visible and false,
// the same as the struct default.
func TestExampleConfig_TrustedProxyOff(t *testing.T) {
	server := yamlChildMap(readExampleRoot(t), "server")
	if server == nil {
		t.Fatal("config.example.yaml has no server block")
	}
	tp := yamlChildScalar(server, "trusted_proxy")
	if tp == nil {
		t.Fatal("config.example.yaml must document server.trusted_proxy")
	}
	var on bool
	if err := tp.Decode(&on); err != nil {
		t.Fatalf("server.trusted_proxy: %v", err)
	}
	if on {
		t.Error("config.example.yaml sets server.trusted_proxy: true; the template must default to false")
	}
}

// TestExampleConfig_WatchdogShowsDefaults: the template's watchdog values are
// what an operator reads as the defaults, so they must be the cliinfo ones.
func TestExampleConfig_WatchdogShowsDefaults(t *testing.T) {
	session := yamlChildMap(readExampleRoot(t), "session")
	if session == nil {
		t.Fatal("config.example.yaml has no session block")
	}
	wd := yamlChildMap(session, "watchdog")
	if wd == nil {
		t.Fatal("config.example.yaml has no session.watchdog block")
	}
	for _, tc := range []struct {
		key  string
		want time.Duration
	}{
		{"no_output_timeout", cliinfo.DefaultNoOutputTimeout},
		{"total_timeout", cliinfo.DefaultTotalTimeout},
	} {
		n := yamlChildScalar(wd, tc.key)
		if n == nil {
			t.Errorf("session.watchdog.%s missing", tc.key)
			continue
		}
		if got, err := time.ParseDuration(n.Value); err != nil || got != tc.want {
			t.Errorf("session.watchdog.%s = %q, want %v (err %v)", tc.key, n.Value, tc.want, err)
		}
	}
}

// watchdogDocRe matches a watchdog budget name followed closely by a duration,
// e.g. `无输出超时 (默认 15min`, `no_output_timeout: "15m"`, `总超时 2h`.
var watchdogDocRe = regexp.MustCompile(`(no_output_timeout|无输出超时|total_timeout|总耗时超时|总超时)[^0-9\n]{0,12}?(\d+)\s*(min|m|h)\b`)

// TestDocs_WatchdogShowsDefaults: every watchdog duration the operator docs
// quote next to a budget name must be the cliinfo default.
func TestDocs_WatchdogShowsDefaults(t *testing.T) {
	for _, doc := range []string{"README.md", "CLAUDE.md", filepath.Join("docs", "design", "DESIGN.md")} {
		data, err := os.ReadFile(filepath.Join("..", "..", doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		found := 0
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range watchdogDocRe.FindAllStringSubmatch(line, -1) {
				found++
				n, _ := strconv.Atoi(m[2])
				got := time.Duration(n) * time.Minute
				if m[3] == "h" {
					got = time.Duration(n) * time.Hour
				}
				want := cliinfo.DefaultTotalTimeout
				if m[1] == "no_output_timeout" || m[1] == "无输出超时" {
					want = cliinfo.DefaultNoOutputTimeout
				}
				if got != want {
					t.Errorf("%s:%d: %q says %v, default is %v", doc, i+1, m[0], got, want)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s quotes no watchdog default; the pattern no longer matches the doc", doc)
		}
	}
}

// readmeBackendEnumRe matches the README config sample's `backend:` line and
// captures its trailing comment, the operator-facing list of valid IDs.
var readmeBackendEnumRe = regexp.MustCompile(`^\s+backend:\s*\S+\s+#(.*)$`)

// TestDocs_READMEListsAllBackends: the README `cli.backend` comment names
// exactly the registered backend IDs, so a new backend cannot ship undocumented.
func TestDocs_READMEListsAllBackends(t *testing.T) {
	withRegisteredBackends(t, func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
		if err != nil {
			t.Fatalf("read README.md: %v", err)
		}
		var got []string
		for _, line := range strings.Split(string(data), "\n") {
			if m := readmeBackendEnumRe.FindStringSubmatch(line); m != nil {
				for _, q := range regexp.MustCompile(`"([a-z0-9_-]+)"`).FindAllStringSubmatch(m[1], -1) {
					got = append(got, q[1])
				}
			}
		}
		if len(got) == 0 {
			t.Fatal("README.md has no `backend: <id>  # \"a\" | \"b\"` line; the pattern no longer matches the doc")
		}
		sort.Strings(got)
		if want := knownBackendIDs(); !slices.Equal(got, want) {
			t.Errorf("README cli.backend comment lists %v, registered backends are %v", got, want)
		}
	})
}

// codexEntryKeys returns, for every `- id: codex` backend entry in the doc
// (commented out or not), the set of keys that entry sets.
func codexEntryKeys(doc string) []map[string]bool {
	uncomment := regexp.MustCompile(`^(\s*)#`)
	var entries []map[string]bool
	dashCol := -1
	for _, raw := range strings.Split(doc, "\n") {
		line := uncomment.ReplaceAllString(raw, "$1 ")
		body := strings.TrimSpace(line)
		col := len(line) - len(strings.TrimLeft(line, " "))
		if strings.HasPrefix(body, "- id:") {
			dashCol = -1
			if f := strings.Fields(strings.TrimPrefix(body, "- id:")); len(f) > 0 && strings.Trim(f[0], `"`) == "codex" {
				dashCol = col
				entries = append(entries, map[string]bool{})
			}
			continue
		}
		if dashCol < 0 || body == "" || strings.HasPrefix(body, "#") {
			continue
		}
		if col <= dashCol {
			dashCol = -1
			continue
		}
		if k, _, ok := strings.Cut(body, ":"); ok {
			entries[len(entries)-1][k] = true
		}
	}
	return entries
}

// TestDocs_CodexSamplesSetModelAndArgs: an omitted per-backend model/args
// inherits cli.model/cli.args, which are claude's, so `codex app-server` would
// get `-c model=sonnet` plus claude flags. Every documented codex entry must
// set both.
func TestDocs_CodexSamplesSetModelAndArgs(t *testing.T) {
	for file, want := range map[string]int{"README.md": 2, "config.example.yaml": 1} {
		data, err := os.ReadFile(filepath.Join("..", "..", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		entries := codexEntryKeys(string(data))
		if len(entries) != want {
			t.Fatalf("%s: found %d codex backend entries, want %d", file, len(entries), want)
		}
		for i, keys := range entries {
			if !keys["model"] || !keys["args"] {
				t.Errorf("%s: codex entry #%d sets %v; it must set both model and args", file, i+1, keys)
			}
		}
	}
}

// yamlChildScalar returns the scalar node for key, or nil.
func yamlChildScalar(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.ScalarNode {
			return m.Content[i+1]
		}
	}
	return nil
}

// TestExampleConfig_RunnerModelHaiku: daemons are short extraction calls, and
// an empty sysession.runner.model runs every one on the CLI's built-in default
// (the main model), so the template a new deployment copies names haiku.
func TestExampleConfig_RunnerModelHaiku(t *testing.T) {
	sys := yamlChildMap(readExampleRoot(t), "sysession")
	if sys == nil {
		t.Fatal("config.example.yaml has no sysession block")
	}
	runner := yamlChildMap(sys, "runner")
	if runner == nil {
		t.Fatal("config.example.yaml has no sysession.runner block")
	}
	model := yamlChildScalar(runner, "model")
	if model == nil {
		t.Fatal("config.example.yaml must document sysession.runner.model")
	}
	if model.Value != "haiku" {
		t.Errorf("sysession.runner.model = %q, want haiku", model.Value)
	}
}
