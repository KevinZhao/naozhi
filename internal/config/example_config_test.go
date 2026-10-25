package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/envpolicy"
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

// TestExampleConfig_NoDeprecatedKeys: an operator copies the template, so any
// schema v1 spelling left in it (`nodes:`, `session.workspace`, the
// agents[].args --append-system-prompt) is a config-deprecated WARN on first
// boot and a setting that silently does nothing.
func TestExampleConfig_NoDeprecatedKeys(t *testing.T) {
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{readExampleRoot(t)}}
	for _, d := range findDeprecatedKeys(doc) {
		t.Errorf("config.example.yaml still uses a deprecated form: %s (%s)", d.Key, d.Reason)
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

// uncommentExampleBlock returns the commented-out YAML that starts at the line
// "<indent>#<key>:" in src, with the one '#' after the indent removed. The
// block runs until a line that is not "<indent>#" plus text.
func uncommentExampleBlock(t *testing.T, src, key string) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		if !strings.HasPrefix(trimmed, "#"+key+":") {
			continue
		}
		prefix := l[:len(l)-len(trimmed)] + "#"
		var out []string
		for _, b := range lines[i:] {
			if !strings.HasPrefix(b, prefix) || len(b) == len(prefix) {
				break
			}
			out = append(out, l[:len(l)-len(trimmed)]+b[len(prefix):])
		}
		return strings.Join(out, "\n") + "\n"
	}
	t.Fatalf("config.example.yaml has no commented %q block", key)
	return ""
}

// TestExampleConfig_AccessProfilesExampleIsValid: the template's commented
// access_profiles block, the multi-backend cli block and the researcher pins,
// uncommented together, must decode with no unknown key and pass the load-time
// access-profile checks, so an operator who uncomments them gets a config
// that loads.
func TestExampleConfig_AccessProfilesExampleIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	src := string(data)
	doc := uncommentExampleBlock(t, src, "default_access_profile") +
		"cli:\n" + uncommentExampleBlock(t, src, "backend") +
		"agents:\n  researcher:\n" + uncommentExampleBlock(t, src, "access_profile")

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("decode uncommented example:\n%s\nerr: %v", doc, err)
	}
	withRegisteredBackends(t, func() {
		if err := validateAccessProfiles(&cfg); err != nil {
			t.Fatalf("validateAccessProfiles: %v", err)
		}
		var withEnv, withModel, withBackend bool
		for name, ap := range cfg.AccessProfiles {
			withEnv = withEnv || len(ap.Env) > 0
			withModel = withModel || ap.DefaultModel != ""
			if ap.DefaultBackend != "" {
				withBackend = true
				if _, ok := backend.Get(ap.DefaultBackend); !ok {
					t.Errorf("access_profiles[%s].default_backend %q is not a registered backend", name, ap.DefaultBackend)
				}
			}
		}
		if !withEnv || !withModel || !withBackend {
			t.Errorf("example profiles must show env (%v), default_model (%v) and default_backend (%v)", withEnv, withModel, withBackend)
		}
		if cfg.DefaultAccessProfile == "" {
			t.Error("example does not show default_access_profile")
		}
		r := cfg.Agents["researcher"]
		if r.AccessProfile == "" || r.Backend == "" {
			t.Errorf("researcher example pins access_profile %q backend %q; want both", r.AccessProfile, r.Backend)
		}
		if _, ok := backend.Get(r.Backend); !ok {
			t.Errorf("researcher backend %q is not a registered backend", r.Backend)
		}
	})
	// The template says AWS_PROFILE is refused in a profile's env.
	if err := envpolicy.ValidateOverlayEntry("AWS_PROFILE", "default"); err == nil {
		t.Error("AWS_PROFILE is accepted in an access-profile env; the template says it fails the load")
	}
	// Every key the allowlist prose names must be overlay-allowed.
	prose := strings.Join(strings.Fields(strings.ReplaceAll(src, "\n#", " ")), " ")
	_, list, ok := strings.Cut(prose, "Only the overlay allowlist is accepted: ")
	list, _, ok2 := strings.Cut(list, " and the Anthropic credentials.")
	if !ok || !ok2 {
		t.Fatal("config.example.yaml lost the overlay allowlist sentence")
	}
	for _, k := range strings.Split(list, ", ") {
		if _, allowed := envpolicy.Allowed(k, envpolicy.SourceOverlay); !allowed {
			t.Errorf("template names %q in the overlay allowlist; the overlay refuses it", k)
		}
	}
}
